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
		Before: func(c echo.Context) {
			path := c.Request().URL.Path
			if c.Request().Method == "POST" && path == "/prompt" {
				sq.Lock()
				defer sq.Unlock()
				sq.Await(servicequeue.CUI)
				sq.CF = &servicequeue.CleanupFunc{
					F: func() {
						http.Post(cuiurl.String()+"/free", echo.MIMEApplicationJSON, bytes.NewBufferString(`{"unload_models":"true","free_memory":"true"}`))
						time.Sleep(time.Second * 5)
					},
					Service: servicequeue.CUI}
			}
		},
		After: func(req *http.Request, resp *http.Response) error {
			if resp.StatusCode != 200 {
				sq.Lock()
				defer sq.Unlock()
				sq.Await(servicequeue.CUI)
				sq.SetService(servicequeue.NONE)
				return nil
			}
			return sq.ServiceCloser(servicequeue.CUI, func(path string) bool {
				return path == "/view"
			}, time.Second*5, true)(req, resp)
		}},
	)
}
