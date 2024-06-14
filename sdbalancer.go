package main

import (
	"net/url"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/rkfg/authproxy/proxy"
	"github.com/rkfg/authproxy/servicequeue"
)

func newSDProxy(target *url.URL) (result echo.MiddlewareFunc) {
	return proxy.NewProxyWrapper(target, nil)
}

func addSDQueueHandlers(e *echo.Echo, sq *servicequeue.ServiceQueue) {
	cleanupFunc := &servicequeue.CleanupFunc{
		F: func() {
			post("/sdapi/v1/unload-checkpoint")
		},
		Service: servicequeue.SD,
	}
	sq.CF = cleanupFunc // SD is supposed to be active by default
	e.POST("/internal/join", func(c echo.Context) error {
		sq.Lock()
		if sq.Await(servicequeue.SD) {
			post("/sdapi/v1/reload-checkpoint")
		}
		sq.CF = cleanupFunc
		sq.Unlock()
		return nil
	})
	e.POST("/internal/leave", func(c echo.Context) error {
		sq.Lock()
		sq.Await(servicequeue.SD)
		sq.SetCleanup(time.Second * 3)
		sq.Unlock()
		return nil
	})
}
