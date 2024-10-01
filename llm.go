package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"time"

	"dario.cat/mergo"
	"github.com/labstack/echo/v4"
	"github.com/rkfg/authproxy/proxy"
	"github.com/rkfg/authproxy/servicequeue"
	"github.com/rkfg/authproxy/watchdog"
)

type llmbalancer struct {
	proxy         echo.MiddlewareFunc
	target        *url.URL
	client        http.Client
	modelName     string
	config        llmConfig
	lastModelName string
	sq            *servicequeue.ServiceQueue
	wd            *watchdog.Watchdog
}

type llmConfig struct {
	Models      map[string]llmArgs `json:"models"`
	DefaultName string             `json:"default_name"`
}

type llmArgs struct {
	NCtx        *uint   `json:"n_ctx"`
	NGpuLayers  *uint   `json:"n-gpu-layers"`
	FlashAttn   *bool   `json:"flash_attn"`
	Tensorcores *bool   `json:"tensorcores"`
	CfgCache    *bool   `json:"cfg_cache"`
	Cache4bit   *bool   `json:"cache_4bit"`
	Loader      *string `json:"loader"`
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

func (l *llmbalancer) unload() {
	log.Printf("Cleanup, restarting the LLM backend")
	l.wd.Send("restart text-generation-webui")
	l.lastModelName = ""
}

func NewLLMBalancer(target *url.URL, sq *servicequeue.ServiceQueue, wd *watchdog.Watchdog) *llmbalancer {
	result := llmbalancer{sq: sq, target: target, wd: wd}
	result.proxy = proxy.NewProxyWrapper(target, &proxy.Interceptor{
		Before: func(c echo.Context) {
			log.Printf("LLM Req: %s %s", c.Request().Method, c.Request().URL.String())
			path := c.Request().URL.Path
			method := c.Request().Method
			if method != "POST" || path != "/v1/chat/completions" && path != "/v1/completions" && path != "/v1/internal/encode" {
				return
			}
			sq.Lock()
			defer sq.Unlock()
			log.Print("LLM sq locked, waiting...")
			sq.Await(servicequeue.LLM, false) // wait until there are no tasks to prevent concurrent model loading
			sq.CF = &servicequeue.CleanupFunc{
				F: func() {
					result.unload()
					time.Sleep(time.Second * 2)
				},
				Service: servicequeue.LLM,
			}
			log.Print("Ensuring the model is loaded")
			err := result.ensureLoaded(c)
			log.Print("Proceeding")
			if err != nil {
				log.Printf("Error loading model: %s", err)
			}
		},
		After: sq.ServiceCloser(servicequeue.LLM, func(path string) bool {
			return path == "/v1/chat/completions" || path == "/v1/completions" || path == "/v1/internal/encode"
		}, time.Second*30, true),
	})
	return &result
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

func (l *llmbalancer) ensureLoaded(c echo.Context) error {
	if c.Request().RequestURI == "/v1/internal/encode" && l.lastModelName != "" {
		return nil
	}
	modelName := "default"
	bodyBytes, err := io.ReadAll(c.Request().Body)
	if err != nil {
		return err
	}
	c.Request().Body = io.NopCloser(bytes.NewBuffer(bodyBytes)) // reset body
	body := map[string]any{}
	err = json.Unmarshal(bodyBytes, &body)
	if err != nil {
		return err
	}
	if model, ok := body["model"].(string); ok && model != "" {
		modelName = model
	}
	args, ok := l.config.Models[modelName]
	if !ok {
		return fmt.Errorf("undefined model %s", modelName)
	}
	if modelName == "default" {
		modelName = l.config.DefaultName
	}
	if l.lastModelName == modelName {
		return nil
	} else {
		log.Printf("Last loaded model is '%s', requested '%s', reloading...", l.lastModelName, modelName)
	}
	if l.lastModelName != "" { // only unload if a model is already loadad
		l.unload()
	}
	var resp TBody
	for retries := 0; retries < 10; retries += 1 {
		time.Sleep(time.Second * 1)
		resp, err = l.post("/v1/internal/model/load", TBody{"model_name": modelName, "args": args})
		if err != nil {
			log.Printf("Error loading model: %s", err)
			continue
		}
		if _, ok := resp["error"]; ok {
			log.Printf("Error loading model: %s", resp)
			continue
		}
		break
	}
	log.Print("Model loaded")
	l.lastModelName = modelName
	return nil
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

func (l *llmbalancer) loadConfig(filename string) error {
	f, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer f.Close()
	err = json.NewDecoder(f).Decode(&l.config)
	if err != nil {
		return err
	}
	for k, v := range l.config.Models {
		if k == "default" {
			continue
		}
		def := l.config.Models["default"]
		err = mergo.Merge(&def, v, mergo.WithOverride, mergo.WithoutDereference)
		if err != nil {
			return err
		}
		l.config.Models[k] = def
	}
	return nil
}
