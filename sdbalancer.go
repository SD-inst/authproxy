package main

import (
	"bytes"
	"encoding/json"
	"io"
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

func isPredict(c echo.Context) bool {
	body, err := io.ReadAll(c.Request().Body)
	if err != nil {
		return false
	}
	c.Request().Body = io.NopCloser(bytes.NewBuffer(body))
	p := map[string]any{}
	err = json.Unmarshal(body, &p)
	if err != nil {
		return false
	}
	if data, ok := p["data"].([]any); ok {
		return len(data) > 0
	}
	return false
}

func (sb *sdbalancer) Next(c echo.Context) *middleware.ProxyTarget {
	if c.Request().URL.Path == "/run/predict" {
		if isPredict(c) {
			sb.sq.Lock()
			defer sb.sq.Unlock()
			if sb.sq.await(SD) && sb.llm != nil {
				sb.llm.unload()
			}
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
		ModifyResponse: sq.serviceCloser(func(path string) bool {
			return path == "/internal/progress"
		}, time.Second*5, false),
	})
	return
}
