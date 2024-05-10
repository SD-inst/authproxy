package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/url"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/rkfg/authproxy/proxy"
)

func isPredict(c echo.Context) bool {
	body, err := io.ReadAll(c.Request().Body)
	if err != nil {
		return false
	}
	c.Request().Body = io.NopCloser(bytes.NewBuffer(body))
	p := map[string]any{}
	err = json.Unmarshal(body, &p)
	if err != nil {
		return false
	}
	if data, ok := p["data"].([]any); ok {
		return len(data) > 0
	}
	return false
}

func newSDProxy(target *url.URL, sq *serviceQueue) (result echo.MiddlewareFunc) {
	result = proxy.NewProxyWrapper(target, &proxy.Interceptor{
		Before: func(c echo.Context) {
			if c.Request().URL.Path == "/run/predict" {
				if isPredict(c) {
					sq.Lock()
					defer sq.Unlock()
					sq.await(SD)
				}
			}

		},
		After: sq.serviceCloser(func(path string) bool {
			return path == "/internal/progress"
		}, time.Second*5, false),
	})
	return
}
