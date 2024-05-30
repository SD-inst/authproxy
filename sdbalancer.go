package main

import (
	"net/url"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/rkfg/authproxy/proxy"
)

func newSDProxy(target *url.URL) (result echo.MiddlewareFunc) {
	return proxy.NewProxyWrapper(target, nil)
}

func addSDQueueHandlers(e *echo.Echo, sq *serviceQueue) {
	cleanupFunc := &cleanupFunc{
		f: func() {
			post("/sdapi/v1/unload-checkpoint")
		},
		service: SD,
	}
	sq.cf = cleanupFunc // SD is supposed to be active by default
	e.POST("/internal/join", func(c echo.Context) error {
		sq.Lock()
		if sq.await(SD) {
			post("/sdapi/v1/reload-checkpoint")
		}
		sq.cf = cleanupFunc
		sq.Unlock()
		return nil
	})
	e.POST("/internal/leave", func(c echo.Context) error {
		sq.Lock()
		sq.await(SD)
		sq.setCleanup(time.Second * 3)
		sq.Unlock()
		return nil
	})
}
