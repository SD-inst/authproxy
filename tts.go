package main

import (
	"log"
	"net/url"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/rkfg/authproxy/proxy"
	"github.com/rkfg/authproxy/watchdog"
)

func newTTSProxy(ttsurl *url.URL, sq *serviceQueue, wd *watchdog.Watchdog) echo.MiddlewareFunc {
	return proxy.NewProxyWrapper(ttsurl, &proxy.Interceptor{
		Before: func(c echo.Context) {
			path := c.Request().URL.Path
			log.Printf("TTS path: %s", path)
			if c.Request().Method == "POST" && path == "/api/generate" || path == "/api/rvc" {
				sq.Lock()
				defer sq.Unlock()
				sq.await(TTS)
				sq.cf = &cleanupFunc{
					f: func() {
						wd.Send("restart tts")
					},
					service: TTS}
			}
		},
		After: sq.serviceCloser(TTS, func(path string) bool {
			return path == "/api/generate" || path == "/api/rvc"
		}, time.Second*5, true)},
	)
}
