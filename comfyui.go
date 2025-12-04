package main

import (
	"bytes"
	"net/http"
	"net/url"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/rkfg/authproxy/proxy"
	"github.com/rkfg/authproxy/servicequeue"
)

func newCUIProxy(cuiurl *url.URL) echo.MiddlewareFunc {
	return proxy.NewProxyWrapper(cuiurl, nil)
}

func addCUIHandlers(e *echo.Echo, sq *servicequeue.ServiceQueue, cuiurl *url.URL) {
	e.POST("/cui/join", func(c echo.Context) error {
		sq.Lock()
		defer sq.Unlock()
		sq.AwaitReent(servicequeue.CUI)
		sq.CF = &servicequeue.CleanupFunc{
			F: func() {
				sq.SetCleanupProgress(false)
				http.Post(cuiurl.String()+"/free", echo.MIMEApplicationJSON, bytes.NewBufferString(`{"unload_models":"true","free_memory":"true"}`))
				sq.WaitForCleanup(time.Second * 20)
			},
			Service: servicequeue.CUI}
		return nil
	})
	e.POST("/cui/leave", func(c echo.Context) error {
		sq.Lock()
		sq.AwaitReent(servicequeue.CUI)
		sq.SetCleanup(time.Second * 3)
		sq.Unlock()
		return nil
	})
}
