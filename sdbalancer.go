package main

import (
	"net/url"

	"github.com/labstack/echo/v4"
	"github.com/rkfg/authproxy/proxy"
)

func newSDProxy(target *url.URL) (result echo.MiddlewareFunc) {
	return proxy.NewProxyWrapper(target, nil)
}
