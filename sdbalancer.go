package main

import (
	"net/http"
	"net/url"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
)

type sdbalancer struct {
	middleware.ProxyBalancer
	sq    *serviceQueue
	llm   *llmbalancer
	proxy echo.MiddlewareFunc
}

func (sb *sdbalancer) Next(c echo.Context) *middleware.ProxyTarget {
	if c.Request().URL.Path == "/run/predict" {
		sb.sq.Lock()
		defer sb.sq.Unlock()
		if sb.sq.await(SD) && sb.llm != nil {
			sb.llm.unload()
		}
	}
	return sb.ProxyBalancer.Next(c)
}

func newSDProxy(target *url.URL, sq *serviceQueue) (result *sdbalancer) {
	result = &sdbalancer{
		ProxyBalancer: middleware.NewRoundRobinBalancer([]*middleware.ProxyTarget{
			{URL: target},
		}),
		sq: sq,
	}
	result.proxy = middleware.ProxyWithConfig(middleware.ProxyConfig{
		Balancer: result,
		ModifyResponse: func(r *http.Response) error {
			if r.Request.URL.Path == "/internal/progress" {
				sq.Lock()
				sq.setCleanup(time.Second * 5)
				sq.Unlock()
			}
			return nil
		},
	})
	return
}
