package main

import (
	"encoding/json"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/donovanhide/eventsource"
	"github.com/labstack/echo/v4"
	"github.com/rkfg/authproxy/metrics"
	"github.com/rkfg/authproxy/proxy"
	"github.com/rkfg/authproxy/servicequeue"
)

type llmbalancer struct {
	proxy         echo.MiddlewareFunc
	target        *url.URL
	client        http.Client
	sq            *servicequeue.ServiceQueue
	metricUpdater chan<- metrics.MetricUpdate
}

func isLLMPath(path string) bool {
	return strings.HasSuffix(path, "/v1/chat/completions") || strings.HasSuffix(path, "/v1/completions") || strings.HasSuffix(path, "/v1/internal/encode") || strings.HasPrefix(path, "/upstream/")
}

func NewLLMBalancer(target *url.URL, sq *servicequeue.ServiceQueue, metricUpdater chan<- metrics.MetricUpdate) *llmbalancer {
	result := llmbalancer{sq: sq, target: target, metricUpdater: metricUpdater}
	result.proxy = proxy.NewProxyWrapper(target, &proxy.Interceptor{
		Before: func(c echo.Context) {
			log.Printf("LLM Req: %s %s", c.Request().Method, c.Request().URL.String())
			path := c.Request().URL.Path
			if !isLLMPath(path) {
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
	go result.startMetricCollection()
	return &result
}

type eventType struct {
	Type string
	Data string
}

type metricType struct {
	ID              uint64  `json:"id"`
	Timestamp       string  `json:"timestamp"`
	Model           string  `json:"model"`
	InputTokens     uint64  `json:"input_tokens"`
	OutputTokens    uint64  `json:"output_tokens"`
	TokensPerSecond float32 `json:"tokens_per_second"`
	DurationMS      uint64  `json:"duration_ms"`
}

func (l *llmbalancer) startMetricCollection() {
	var stream *eventsource.Stream
	var err error
	for stream == nil {
		stream, err = eventsource.Subscribe(l.target.JoinPath("/api/events").String(), "")
		if err != nil {
			log.Printf("Error joining llama-swap event stream: %s; retrying...", err)
		}
		time.Sleep(time.Second * 5)
	}
	cutoff := time.Now()
	for {
		select {
		case event := <-stream.Events:
			if event.Event() != "message" {
				break
			}
			e := eventType{}
			err = json.Unmarshal([]byte(event.Data()), &e)
			if err != nil {
				log.Printf("Error unmarshalling event: %s; error: %s", event.Data(), err)
				break
			}
			if e.Type != "metrics" {
				break
			}
			marr := []metricType{}
			err = json.Unmarshal([]byte(e.Data), &marr)
			if err != nil {
				log.Printf("Error unmarshalling metric: %s; error: %s", e.Data, err)
				break
			}
			for _, m := range marr {
				ts, err := time.Parse(time.RFC3339Nano, m.Timestamp)
				if err != nil {
					log.Printf("Error parsing timestamp %s: %s", m.Timestamp, err)
				}
				if ts.After(cutoff) {
					log.Printf("Tokens generated: %d", m.OutputTokens)
					l.metricUpdater <- metrics.MetricUpdate{Type: metrics.LLM_TOKENS, Value: float64(m.OutputTokens)}
				}
			}
		case err := <-stream.Errors:
			log.Printf("Error: %s", err)
		}
	}
}

func (l *llmbalancer) forbidden(c echo.Context) error {
	return JSONErrorMessage(c, 403, "forbidden")
}
