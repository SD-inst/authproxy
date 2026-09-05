package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
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
	return (strings.HasSuffix(path, "/v1/chat/completions") || strings.HasSuffix(path, "/v1/completions") ||
		strings.HasSuffix(path, "/v1/internal/encode") || strings.HasSuffix(path, "/v1/embeddings") || strings.HasPrefix(path, "/upstream/")) && !strings.HasSuffix(path, ".js")
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
				path := c.Request().URL.Path
				if m, ok := strings.CutPrefix(path, "/upstream/"); ok {
					model = m
					if idx := strings.Index(model, "/v1/"); idx >= 0 {
						model = model[:idx]
					}
				}
				if model == "" {
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
				}
				sq.AwaitWithPredicateAndDescription(servicequeue.LLM, true, func() bool {
					if apiKey != prevKey {
						log.Printf("API key mismatch: '%s' != '%s'", apiKey, prevKey)
					}
					if result.model != "" && prevModel != model {
						log.Printf("Model mismatch: '%s' != '%s'", model, prevModel)
					}
					return apiKey == prevKey && (result.model == "" || prevModel == model)
				}, model)
				// don't need to make it a CV as we rely on service queue mutex
				result.apiKey = apiKey
				result.model = model
			} else {
				sq.Await(servicequeue.LLM, false)
			}
			sq.Hold()
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
		}, time.Second*300, true, func(req *http.Request) time.Duration {
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

// activityEventID is the payload of an "activity" SSE event (llama-swap v240+):
// only the store row id; the full row is fetched from /api/metrics/activity.
type activityEventID struct {
	ID int `json:"id"`
}

// activityPage is the response envelope of GET /api/metrics/activity.
type activityPage struct {
	Data  []metricType `json:"data"`
	Total int          `json:"total"`
}

func (l *llmbalancer) startMetricCollection() {
	var stream *eventsource.Stream
	var err error
	for stream == nil {
		stream, err = eventsource.Subscribe(l.target.JoinPath("/api/events").String(), "")
		if err == nil {
			break
		}
		time.Sleep(time.Second * 5)
	}
	cutoff := time.Now()
	var lastID int
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
			// llama-swap v240+: activity events are named "activity" and carry
			// only a row id; the row (with tokens) is fetched from the activity API.
			if e.Type != "activity" {
				break
			}
			var evt activityEventID
			err = json.Unmarshal([]byte(e.Data), &evt)
			if err != nil {
				log.Printf("Error unmarshalling activity event: %s; error: %s", e.Data, err)
				break
			}
			// High-water mark: skip rows we already processed and pick up rows
			// for events we missed (e.g. SSE reconnect).
			if evt.ID <= lastID {
				break
			}
			rows, err := l.fetchActivityRows(lastID+1, evt.ID)
			if err != nil {
				log.Printf("Error fetching activity rows: %s", err)
				break
			}
			for _, m := range rows {
				// The server may return more rows than the requested
				// [min_id, max_id] range (older llama-swap versions ignore
				// min_id), so dedup on the client side against the high-water
				// mark: only rows newer than lastID are processed.
				if m.ID <= uint64(lastID) {
					continue
				}
				ts, perr := time.Parse(time.RFC3339Nano, m.Timestamp)
				if perr != nil {
					log.Printf("Error parsing timestamp %s: %s", m.Timestamp, perr)
					continue
				}
				if ts.After(cutoff) {
					log.Printf("Tokens generated: %d", m.Tokens.OutputTokens)
					l.metricUpdater <- metrics.MetricUpdate{Type: metrics.LLM_TOKENS, Value: float64(m.Tokens.OutputTokens)}
				}
			}
			lastID = evt.ID
		case <-stream.Errors:
			continue
		}
	}
}

// fetchActivityRows returns activity rows with ids in [minID, maxID] from
// llama-swap's GET /api/metrics/activity endpoint (llama-swap v240+).
func (l *llmbalancer) fetchActivityRows(minID, maxID int) ([]metricType, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	u := l.target.JoinPath("/api/metrics/activity")
	q := u.Query()
	q.Set("min_id", strconv.Itoa(minID))
	q.Set("max_id", strconv.Itoa(maxID))
	q.Set("limit", "999")
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := l.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("activity API returned status %d", resp.StatusCode)
	}
	var page activityPage
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		return nil, err
	}
	return page.Data, nil
}

func (l *llmbalancer) forbidden(c echo.Context) error {
	return JSONErrorMessage(c, 403, "forbidden")
}

func (l *llmbalancer) lookup(c echo.Context) error {
	return c.JSON(http.StatusOK, []string{})
}

func (l *llmbalancer) tools(c echo.Context) error {
	return c.JSON(http.StatusForbidden, map[string]any{
		"error": map[string]string{
			"message": "this feature is disabled",
			"type":    "feature_disabled",
		},
	})
}
