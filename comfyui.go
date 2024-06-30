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

func newCUIProxy(cuiurl *url.URL, sq *servicequeue.ServiceQueue) echo.MiddlewareFunc {
	return proxy.NewProxyWrapper(cuiurl, &proxy.Interceptor{
		After: func(req *http.Request, resp *http.Response) error {
			if resp.StatusCode >= 400 {
				sq.Lock()
				defer sq.Unlock()
				sq.Await(servicequeue.CUI)
				sq.SetService(servicequeue.NONE)
			}
			return nil
		}},
	)
}

func addCUIHandlers(e *echo.Echo, sq *servicequeue.ServiceQueue, cuiurl *url.URL) {
	e.POST("/cui/join", func(c echo.Context) error {
		sq.Lock()
		defer sq.Unlock()
		sq.Await(servicequeue.CUI)
		sq.CF = &servicequeue.CleanupFunc{
			F: func() {
				http.Post(cuiurl.String()+"/free", echo.MIMEApplicationJSON, bytes.NewBufferString(`{"unload_models":"true","free_memory":"true"}`))
				time.Sleep(time.Second * 5)
			},
			Service: servicequeue.CUI}
		return nil
	})
	e.POST("/cui/leave", func(c echo.Context) error {
		sq.Lock()
		sq.Await(servicequeue.CUI)
		sq.SetCleanup(time.Second * 3)
		sq.Unlock()
		return nil
	})
}
