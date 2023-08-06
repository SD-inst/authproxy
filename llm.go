package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
)

type llmbalancer struct {
	middleware.ProxyBalancer
	target      *url.URL
	stream      *url.URL
	client      http.Client
	timeoutMins int
	modelName   string
	args        any
	timeout     *time.Timer
}

type TBody map[string]any

func parseBody(resp io.ReadCloser) (TBody, error) {
	b := TBody{}
	return b, json.NewDecoder(resp).Decode(&b)
}

func NewLLMBalancer(target *url.URL, stream *url.URL) *llmbalancer {
	var result llmbalancer
	result.ProxyBalancer = middleware.NewRoundRobinBalancer([]*middleware.ProxyTarget{{URL: target}})
	result.target = target
	result.stream = stream
	return &result
}

func (l *llmbalancer) updateTimeout() {
	if l.timeout != nil {
		l.timeout.Stop()
	}
	l.timeout = time.AfterFunc(time.Minute*time.Duration(l.timeoutMins), func() {
		log.Printf("Timed out, unloading the model")
		l.post("/api/v1/model", TBody{"action": "unload"})
	})
}

func (l *llmbalancer) post(path string, body TBody) (TBody, error) {
	buf := bytes.Buffer{}
	json.NewEncoder(&buf).Encode(body)
	log.Printf("POST params: %s", buf.String())
	resp, err := l.client.Post(l.target.JoinPath(path).String(), "application/json", &buf)
	if err != nil {
		return nil, err
	}
	return parseBody(resp.Body)
}

func (l *llmbalancer) ensureLoaded() error {
	l.updateTimeout()
	_, err := l.post("/api/v1/token-count", TBody{"prompt": "ping"})
	if err != nil && err.Error() == "EOF" {
		log.Print("Model unloaded, reloading...")
		resp, err := l.post("/api/v1/model", TBody{"action": "load", "model_name": l.modelName, "args": l.args})
		if err != nil {
			return err
		}
		if err, ok := resp["error"]; ok {
			errmsg := err.(map[string]any)["message"]
			log.Printf("Error loading model: %s", resp)
			return fmt.Errorf("%s", errmsg)
		} else {
			log.Print("Model loaded")
		}
	}
	return nil
}

func (l *llmbalancer) NextTarget(c echo.Context) (*middleware.ProxyTarget, error) {
	log.Printf("Req: %s %s", c.Request().Method, c.Request().URL.String())
	path := c.Request().URL.Path
	method := c.Request().Method
	if method == "GET" && path == "/api/v1/stream" {
		l.ensureLoaded()
		if l.stream != nil && l.stream.Host != "" {
			return &middleware.ProxyTarget{URL: l.stream}, nil
		}
	}
	if method == "POST" && path == "/api/v1/generate" {
		l.ensureLoaded()
	}
	return l.ProxyBalancer.Next(c), nil
}

func (l *llmbalancer) handleModel(c echo.Context) error {
	return c.JSON(200, TBody{"result": l.modelName})
}
