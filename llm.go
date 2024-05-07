package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
)

type llmbalancer struct {
	middleware.ProxyBalancer
	target      *url.URL
	client      http.Client
	timeoutMins int
	modelName   string
	loraNames   []string
	args        any
	sq          *serviceQueue
	proxy       echo.MiddlewareFunc
}

type bodyWrapper struct {
	io.ReadCloser
	onClose func()
}

func (b bodyWrapper) Close() error {
	if b.onClose != nil {
		b.onClose()
	}
	return b.ReadCloser.Close()
}

type TBody map[string]any

func parseBody(resp io.ReadCloser) (TBody, error) {
	b := TBody{}
	bytes, err := io.ReadAll(resp)
	if err != nil {
		return nil, err
	}
	err = json.Unmarshal(bytes, &b)
	if err != nil {
		return TBody{"result": string(bytes)}, nil
	}
	return b, nil
}

func NewLLMBalancer(target *url.URL, sq *serviceQueue) *llmbalancer {
	var result llmbalancer
	result.ProxyBalancer = middleware.NewRoundRobinBalancer([]*middleware.ProxyTarget{{URL: target}})
	result.sq = sq
	result.target = target
	result.proxy = middleware.ProxyWithConfig(middleware.ProxyConfig{
		Balancer: &result,
		ModifyResponse: func(r *http.Response) error {
			path := r.Request.URL.Path
			if path == "/v1/chat/completions" || path == "/v1/completions" {
				r.Body = bodyWrapper{ReadCloser: r.Body, onClose: func() {
					sq.Lock()
					sq.cancelCleanup()
					sq.setService(NONE)
					sq.Unlock()
				}}
				sq.Lock()
				sq.setCleanup(time.Second * 5)
				sq.Unlock()
			}
			return nil
		},
	})
	return &result
}

func (l *llmbalancer) unload() {
	log.Printf("Unloading the model")
	l.post("/v1/internal/model/unload", TBody{})
}

func (l *llmbalancer) post(path string, body TBody) (TBody, error) {
	buf := bytes.Buffer{}
	json.NewEncoder(&buf).Encode(body)
	log.Printf("POST URL: %s params: %s", path, buf.String())
	resp, err := l.client.Post(l.target.JoinPath(path).String(), "application/json", &buf)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("error code %d", resp.StatusCode)
	}
	return parseBody(resp.Body)
}

func (l *llmbalancer) ensureLoaded() error {
	_, err := l.post("/v1/internal/token-count", TBody{"text": "ping"})
	if err == nil {
		return nil
	}
	log.Print("Model unloaded, reloading...")
	resp, err := l.post("/v1/internal/model/load", TBody{"model_name": l.modelName, "args": l.args})
	if err != nil {
		log.Printf("Error loading model: %s", err)
		return err
	}
	if err, ok := resp["error"]; ok {
		log.Printf("Error loading model: %s", resp)
		return fmt.Errorf("%s", err)
	}
	log.Print("Model loaded")
	if len(l.loraNames) > 0 {
		resp, err = l.post("/v1/internal/lora/load", TBody{"lora_names": l.loraNames})
		if err != nil {
			log.Printf("Error loading loras %s: %s", strings.Join(l.loraNames, ", "), err)
			return err
		}
		if err, ok := resp["error"]; ok {
			log.Printf("Error loading loras %s: %s", strings.Join(l.loraNames, ", "), resp)
			return fmt.Errorf("%s", err)
		}
	}
	return nil
}

func (l *llmbalancer) NextTarget(c echo.Context) (*middleware.ProxyTarget, error) {
	log.Printf("Req: %s %s", c.Request().Method, c.Request().URL.String())
	path := c.Request().URL.Path
	method := c.Request().Method
	if method == "POST" && (path == "/v1/chat/completions" || path == "/v1/completions") {
		l.sq.Lock()
		defer l.sq.Unlock()
		l.sq.await(LLM)
		l.ensureLoaded()
	}
	return l.ProxyBalancer.Next(c), nil
}

func (l *llmbalancer) handleModel(c echo.Context) error {
	return c.JSON(200, TBody{"model_name": l.modelName})
}

func (l *llmbalancer) handleModels(c echo.Context) error {
	return c.JSON(200, TBody{
		"object": "list",
		"data": []map[string]any{{
			"id":       l.modelName,
			"object":   "model",
			"created":  0,
			"owned_by": "user",
		}}})
}

func (l *llmbalancer) forbidden(c echo.Context) error {
	return JSONErrorMessage(c, 403, "forbidden")
}
