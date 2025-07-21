package main

import (
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/rkfg/authproxy/proxy"
	"github.com/rkfg/authproxy/servicequeue"
)

type llmbalancer struct {
	proxy  echo.MiddlewareFunc
	target *url.URL
	client http.Client
	sq     *servicequeue.ServiceQueue
}

func isLLMPath(path string) bool {
	return strings.HasSuffix(path, "/v1/chat/completions") || strings.HasSuffix(path, "/v1/completions") || strings.HasSuffix(path, "/v1/internal/encode")
}

func NewLLMBalancer(target *url.URL, sq *servicequeue.ServiceQueue) *llmbalancer {
	result := llmbalancer{sq: sq, target: target}
	result.proxy = proxy.NewProxyWrapper(target, &proxy.Interceptor{
		Before: func(c echo.Context) {
			log.Printf("LLM Req: %s %s", c.Request().Method, c.Request().URL.String())
			path := c.Request().URL.Path
			method := c.Request().Method
			if method != "POST" || !isLLMPath(path) {
				return
			}
			sq.Lock()
			defer sq.Unlock()
			log.Print("LLM sq locked, waiting...")
			sq.Await(servicequeue.LLM, false) // wait until there are no tasks to prevent concurrent model loading
			sq.CF = &servicequeue.CleanupFunc{
				F: func() {
					result.client.Get(result.target.JoinPath("/unload").String())
				},
				Service: servicequeue.LLM,
			}
		},
		After: sq.ServiceCloser(servicequeue.LLM, func(path string) bool {
			return isLLMPath(path)
		}, time.Second*30, true),
	})
	return &result
}

func (l *llmbalancer) forbidden(c echo.Context) error {
	return JSONErrorMessage(c, 403, "forbidden")
}
