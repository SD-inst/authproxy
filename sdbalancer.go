package main

import (
	"net/url"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/rkfg/authproxy/proxy"
)

func newSDProxy(target *url.URL, sq *serviceQueue) (result echo.MiddlewareFunc) {
	result = proxy.NewProxyWrapper(target, &proxy.Interceptor{
		Before: func(c echo.Context) {
			path := c.Request().URL.Path
			if path == "/queue/join" {
				sq.Lock()
				defer sq.Unlock()
				sq.await(SD)
			}
		},
		After: sq.serviceCloser(SD, func(path string) bool {
			return path == "/internal/progress"
		}, time.Second*5, false),
	})
	return
}
