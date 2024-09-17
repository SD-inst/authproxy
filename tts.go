package main

import (
	"net/url"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/rkfg/authproxy/proxy"
	"github.com/rkfg/authproxy/servicequeue"
	"github.com/rkfg/authproxy/watchdog"
)

func newTTSProxy(ttsurl *url.URL, sq *servicequeue.ServiceQueue, wd *watchdog.Watchdog) echo.MiddlewareFunc {
	return proxy.NewProxyWrapper(ttsurl, &proxy.Interceptor{
		Before: func(c echo.Context) {
			path := c.Request().URL.Path
			if c.Request().Method == "POST" && path == "/api/generate" || path == "/api/rvc" {
				sq.Lock()
				defer sq.Unlock()
				sq.AwaitReent(servicequeue.TTS)
				sq.CF = &servicequeue.CleanupFunc{
					F: func() {
						wd.Send("restart tts")
					},
					Service: servicequeue.TTS}
			}
		},
		After: sq.ServiceCloser(servicequeue.TTS, func(path string) bool {
			return path == "/api/generate" || path == "/api/rvc"
		}, time.Second*5, true)},
	)
}
