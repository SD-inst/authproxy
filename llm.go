package main

import (
	"bytes"
	"encoding/json"
	"io"
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
	apiKey        string
	model         string
}

func isLLMPath(path string) bool {
	return strings.HasSuffix(path, "/v1/chat/completions") || strings.HasSuffix(path, "/v1/completions") ||
		strings.HasSuffix(path, "/v1/internal/encode") || strings.HasSuffix(path, "/v1/embeddings") || strings.HasPrefix(path, "/upstream/")
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
			// Convert WebP to PNG in request body for VLM images
			if err := proxy.ConvertRequestIfNeeded(c); err != nil {
				log.Printf("Error converting request images: %s", err)
				// Don't fail the request, just log the error
			}
			sq.Lock()
			defer sq.Unlock()
			log.Print("LLM sq locked, waiting...")
			// wait until there are no tasks to prevent concurrent model loading
			// only allow call chaining with POST methods
			if c.Request().Method == "POST" {
				// this can proceed if the service is either NONE or WAIT/LLM and the API apiKey matches (allow consequent requests from the same user to go uninterrupted)
				apiKey := c.Request().Header.Get("Authorization")
				prevKey := result.apiKey
				prevModel := result.model
				body, err := io.ReadAll(c.Request().Body)
				c.Request().Body = io.NopCloser(bytes.NewBuffer(body))
				model := ""
				if err != nil {
					log.Printf("Error reading LLM request body: %s", err)
				} else {
					req := struct {
						Model string
					}{}
					if err := json.Unmarshal(body, &req); err != nil {
						log.Printf("Error parsing LLM request body: %s", err)
					} else {
						model = req.Model
					}
				}
				sq.AwaitWithPredicate(servicequeue.LLM, true, func() bool {
					if apiKey != prevKey {
						log.Printf("API key mismatch: '%s' != '%s'", apiKey, prevKey)
					}
					if result.model != "" && prevModel != model {
						log.Printf("Model mismatch: '%s' != '%s'", model, prevModel)
					}
					return apiKey == prevKey && (result.model == "" || prevModel == model)
				})
				// don't need to make it a CV as we rely on service queue mutex
				result.apiKey = apiKey
				result.model = model
				sq.SendDescriptionUpdate(servicequeue.LLM, model)
			} else {
				sq.Await(servicequeue.LLM, false)
			}
			sq.CancelCleanup() // cancel potential WAIT/LLM cleanup
			sq.CF = &servicequeue.CleanupFunc{
				F: func() {
					result.client.Get(result.target.JoinPath("/unload").String())
				},
				Service: servicequeue.LLM,
			}
		},
		After: sq.ServiceCloserWithAfterBody(servicequeue.LLM, func(path string) bool {
			return isLLMPath(path)
		}, time.Second*120, true, func(req *http.Request) time.Duration {
			if (strings.Contains(req.URL.Path, "/completions") || strings.Contains(req.URL.Path, "/embeddings")) && req.Method == "POST" {
				return time.Second * 10
			}
			return 0
		}),
	})
	go result.startMetricCollection()
	return &result
}

type eventType struct {
	Type string
	Data string
}

type metricType struct {
	ID        uint64 `json:"id"`
	Timestamp string `json:"timestamp"`
	Model     string `json:"model"`
	Tokens    struct {
		InputTokens     uint64  `json:"input_tokens"`
		OutputTokens    uint64  `json:"output_tokens"`
		TokensPerSecond float32 `json:"tokens_per_second"`
	}
	DurationMS uint64 `json:"duration_ms"`
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
					log.Printf("Tokens generated: %d", m.Tokens.OutputTokens)
					l.metricUpdater <- metrics.MetricUpdate{Type: metrics.LLM_TOKENS, Value: float64(m.Tokens.OutputTokens)}
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
