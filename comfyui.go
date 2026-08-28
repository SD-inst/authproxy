package main

import (
	"bytes"
	"net/http"
	"net/url"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/rkfg/authproxy/progress"
	"github.com/rkfg/authproxy/proxy"
	"github.com/rkfg/authproxy/servicequeue"
)

func newCUIProxy(cuiurl *url.URL) echo.MiddlewareFunc {
	return proxy.NewProxyWrapper(cuiurl, nil)
}

func addCUIHandlers(e *echo.Echo, sq *servicequeue.ServiceQueue, cuiurl *url.URL, pr progress.CUIReseter) {
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
		sq.SetService(servicequeue.CUI, "preparing...")
		// ComfyUI does not send a zero-progress on task start, so reset the
		// previous task's progress and ETA baselines here (otherwise the
		// progress lingers at 100% until the next sampler's first step).
		pr.ResetCUI()
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
