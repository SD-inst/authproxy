package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/labstack/echo/v4"
)

// adminToken signs an sdkey JWT for the given login, the way the login handler
// does, so adminOnly sees that user.
func adminToken(t *testing.T, login string) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.RegisteredClaims{
		Subject:   login,
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
	})
	signed, err := token.SignedString([]byte(params.JWTSecret))
	if err != nil {
		t.Fatalf("sign token: %s", err)
	}
	return signed
}

// newAdminApp mirrors the route setup of main.go: the admin routes are
// registered before the catch-all domain proxy, which stands in for the
// dispatcher that would wake A1111 for any path it sees (it bumps woken).
func newAdminApp(m *containerManager, woken *int) *echo.Echo {
	e := echo.New()
	e.HideBanner = true
	addAdminHandlers(e, m)
	e.Group("/*", func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			*woken++
			return c.String(http.StatusBadGateway, "would start stablediff-cuda")
		}
	})
	return e
}

func doRequest(t *testing.T, e *echo.Echo, method, path, login, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if login != "" {
		req.AddCookie(&http.Cookie{Name: cookieName, Value: adminToken(t, login)})
	}
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

func withJWTSecret(t *testing.T) {
	t.Helper()
	old := params.JWTSecret
	params.JWTSecret = "testsecret"
	t.Cleanup(func() { params.JWTSecret = old })
}

func TestAdminPageIsAdminOnly(t *testing.T) {
	withJWTSecret(t)
	m := newTestManager(&fakeWd{enabled: true, resp: "ok"})
	var woken int
	e := newAdminApp(m, &woken)

	for _, tc := range []struct {
		login string
		want  int
	}{
		{"", http.StatusForbidden},
		{"alice", http.StatusForbidden},
		{"admin", http.StatusOK},
	} {
		rec := doRequest(t, e, http.MethodGet, "/admin/", tc.login, "")
		if rec.Code != tc.want {
			t.Fatalf("GET /admin/ as %q: got %d, want %d", tc.login, rec.Code, tc.want)
		}
	}
	if woken != 0 {
		t.Fatalf("the admin page must never reach the domain proxy, it did %d times", woken)
	}

	rec := doRequest(t, e, http.MethodGet, "/admin/", "admin", "")
	if !strings.Contains(rec.Body.String(), "<title>Containers</title>") {
		t.Fatal("the admin page was not served")
	}
}

func TestAdminStatusEndpoint(t *testing.T) {
	withJWTSecret(t)
	m := newTestManager(&fakeWd{enabled: true, resp: "ok"})
	var woken int
	e := newAdminApp(m, &woken)

	rec := doRequest(t, e, http.MethodGet, "/admin/containers", "admin", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var s containerStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &s); err != nil {
		t.Fatalf("decode status: %s", err)
	}
	if len(s.Services) != len(managedServices) {
		t.Fatalf("got %d services, want %d", len(s.Services), len(managedServices))
	}
	if woken != 0 {
		t.Fatal("the status endpoint must not reach the domain proxy")
	}
}

func TestAdminControlEndpoint(t *testing.T) {
	withJWTSecret(t)
	wd := &fakeWd{enabled: true, resp: "ok"}
	m := newTestManager(wd)
	var woken int
	e := newAdminApp(m, &woken)

	rec := doRequest(t, e, http.MethodPost, "/admin/container", "admin", `{"service":"comfyui","op":"stop"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	if len(wd.commands) != 1 || wd.commands[0] != "stop comfyui" {
		t.Fatalf("watchdog commands = %v", wd.commands)
	}
	// The response carries the fresh status so the page needs no second request.
	var s containerStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &s); err != nil {
		t.Fatalf("decode status: %s", err)
	}

	rec = doRequest(t, e, http.MethodPost, "/admin/container", "alice", `{"service":"comfyui","op":"stop"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a non-admin must get 403, got %d", rec.Code)
	}
	if len(wd.commands) != 1 {
		t.Fatalf("a forbidden request must not act, commands = %v", wd.commands)
	}

	rec = doRequest(t, e, http.MethodPost, "/admin/container", "admin", `{"service":"comfyui","op":"destroy"}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("an unknown op must be rejected, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(wd.commands) != 1 {
		t.Fatalf("an unknown op must not reach the watchdog, commands = %v", wd.commands)
	}
}

// An unknown path under /admin must 404 inside the admin routes. Left to the
// catch-all, a typo in the URL would start A1111.
func TestAdminUnknownPathDoesNotWakeContainers(t *testing.T) {
	withJWTSecret(t)
	m := newTestManager(&fakeWd{enabled: true, resp: "ok"})
	var woken int
	e := newAdminApp(m, &woken)

	rec := doRequest(t, e, http.MethodGet, "/admin/nonexistent", "admin", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	if woken != 0 {
		t.Fatalf("the domain proxy ran %d times for a path under /admin", woken)
	}

	rec = doRequest(t, e, http.MethodGet, "/admin/nonexistent", "", "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("an unknown admin path must not leak anything to anonymous users, got %d", rec.Code)
	}
}
