package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
)

// The converter consumes the body of a chat completion to look for images, and
// a request whose body it could not use is still a request: the caller logs the
// conversion failure and proxies what it got. So the body must come back
// readable on every path out of the function, not only on the one that happens
// to find a message list.
func TestConvertRequestIfNeededKeepsTheBodyReadable(t *testing.T) {
	// A 1x1 PNG, and a base64 payload that is not a WebP at all.
	const pngURL = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

	tests := []struct {
		name string
		path string
		body string
	}{
		{"no messages at all", "/v1/chat/completions", `{"model":"llama"}`},
		{"not JSON", "/v1/chat/completions", `hello`},
		{"messages is not a list", "/v1/chat/completions", `{"model":"llama","messages":"hi"}`},
		{"messages are not objects", "/v1/chat/completions", `{"model":"llama","messages":[1,2]}`},
		{"a plain chat", "/v1/chat/completions", `{"model":"llama","messages":[{"role":"user","content":"hi"}]}`},
		{"a PNG image needs no conversion", "/v1/chat/completions",
			`{"model":"vision","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"` + pngURL + `"}}]}]}`},
		{"a broken WebP is left to the upstream", "/v1/chat/completions",
			`{"model":"vision","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/webp;base64,AAAA"}}]}]}`},
		{"a path the converter ignores", "/v1/completions", `{"model":"llama","prompt":"hi"}`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := echo.New()
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			c := e.NewContext(req, httptest.NewRecorder())

			// An error here is the caller's log line, not a reason to lose the body.
			_ = ConvertRequestIfNeeded(c)

			got, err := io.ReadAll(c.Request().Body)
			if err != nil {
				t.Fatalf("the body must still be readable after the converter looked at it: %s", err)
			}
			if string(got) != tc.body {
				t.Errorf("the body must reach the upstream as it arrived: got %q, want %q", got, tc.body)
			}
			if c.Request().ContentLength != int64(len(tc.body)) {
				t.Errorf("Content-Length must match the body the proxy will send: got %d, want %d",
					c.Request().ContentLength, len(tc.body))
			}
		})
	}
}

// A request that is not a POST has no body to convert and must not be touched,
// and neither has a POST on a path the converter does not serve.
func TestConvertRequestIfNeededLeavesOtherRequestsAlone(t *testing.T) {
	e := echo.New()

	get := httptest.NewRequest(http.MethodGet, "/v1/chat/completions", nil)
	getC := e.NewContext(get, httptest.NewRecorder())
	if err := ConvertRequestIfNeeded(getC); err != nil {
		t.Errorf("a GET has nothing to convert, got %s", err)
	}
	if getC.Request().Body != nil && getC.Request().Body != http.NoBody {
		t.Errorf("a GET must be left alone, its body was replaced")
	}

	post := httptest.NewRequest(http.MethodPost, "/v1/models", strings.NewReader(`{"a":1}`))
	bodyBefore := post.Body
	postC := e.NewContext(post, httptest.NewRecorder())
	if err := ConvertRequestIfNeeded(postC); err != nil {
		t.Errorf("a path outside chat completions has nothing to convert, got %s", err)
	}
	if postC.Request().Body != bodyBefore {
		t.Errorf("a chat-completions-only converter must not replace the body of %s", post.URL.Path)
	}
}
