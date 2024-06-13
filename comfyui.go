package main

import (
	"bytes"
	"net/http"
	"net/url"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/rkfg/authproxy/proxy"
)

func newCUIProxy(cuiurl *url.URL, sq *serviceQueue) echo.MiddlewareFunc {
	return proxy.NewProxyWrapper(cuiurl, &proxy.Interceptor{
		Before: func(c echo.Context) {
			path := c.Request().URL.Path
			if c.Request().Method == "POST" && path == "/prompt" {
				sq.Lock()
				defer sq.Unlock()
				sq.await(CUI)
				sq.cf = &cleanupFunc{
					f: func() {
						http.Post(cuiurl.String()+"/free", echo.MIMEApplicationJSON, bytes.NewBufferString(`{"unload_models":"true","free_memory":"true"}`))
						time.Sleep(time.Second * 5)
					},
					service: CUI}
			}
		},
		After: func(req *http.Request, resp *http.Response) error {
			if resp.StatusCode != 200 {
				sq.Lock()
				defer sq.Unlock()
				sq.await(CUI)
				sq.setService(NONE)
				return nil
			}
			return sq.serviceCloser(CUI, func(path string) bool {
				return path == "/view"
			}, time.Second*5, true)(req, resp)
		}},
	)
}
