package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/rkfg/authproxy/servicequeue"
)

var (
	upstreamAddr  = os.Getenv("LLM_PROXY_UPSTREAM")
	listenAddr    = os.Getenv("LLM_PROXY_LISTEN")
	defaultAPIKey = os.Getenv("LLM_PROXY_DEFAULT_API_KEY")
	// gracePeriod is how long the model stays loaded after the last request's
	// response body closes, so consecutive requests from the same user go
	// uninterrupted. Mirrors llm.go's waitAfterBody.
	gracePeriod = parseDuration(os.Getenv("LLM_PROXY_GRACE_PERIOD"), 10*time.Second)
	// hardCap is the safety net: it frees the slot even if a response body never
	// closes (stuck upstream connection). Mirrors llm.go's cleanup timeout.
	hardCap = parseDuration(os.Getenv("LLM_PROXY_HOLD_TIMEOUT"), 5*time.Minute)
)

func parseDuration(s string, def time.Duration) time.Duration {
	if s == "" {
		return def
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		log.Printf("Invalid duration %q, using %s", s, def)
		return def
	}
	return d
}

// llmProxy holds the shared state. apiKey/model are guarded by the ServiceQueue
// mutex (they are only touched while sq is locked).
type llmProxy struct {
	sq          *servicequeue.ServiceQueue
	target      *url.URL
	client      http.Client
	apiKey      string
	model       string
	heldSince   time.Time
	heldRequest int
}

func isLLMPath(path string) bool {
	return (strings.HasSuffix(path, "/v1/chat/completions") || strings.HasSuffix(path, "/v1/completions") ||
		strings.HasSuffix(path, "/v1/internal/encode") || strings.HasSuffix(path, "/v1/embeddings") ||
		strings.HasPrefix(path, "/upstream/")) && !strings.HasSuffix(path, ".js")
}

// isLookupPath matches the /upstream/:model/v1/streams/lookup route.
// Any method: the web UI polls it, and a method-restricted stub leaked traffic.
func isLookupPath(path string) bool {
	return strings.HasPrefix(path, "/upstream/") &&
		strings.HasSuffix(path, "/v1/streams/lookup")
}

// isToolsPath matches the /upstream/:model/tools route.
// Any method: the web UI polls it with POST, which a GET-only stub let through.
func isToolsPath(path string) bool {
	return strings.HasPrefix(path, "/upstream/") &&
		strings.HasSuffix(path, "/tools")
}

// graceDelay mirrors llm.go's waitAfterBody
func graceDelay(r *http.Request) time.Duration {
	if r.Method == "POST" &&
		(strings.Contains(r.URL.Path, "/completions") || strings.Contains(r.URL.Path, "/embeddings")) {
		return gracePeriod
	}
	return 0
}

func extractModel(r *http.Request) string {
	path := r.URL.Path

	if m, ok := strings.CutPrefix(path, "/upstream/"); ok {
		model := m
		if idx := strings.Index(model, "/v1/"); idx >= 0 {
			model = model[:idx]
		}
		return model
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Printf("extractModel: reading body: %s", err)
		return ""
	}
	r.Body = io.NopCloser(bytes.NewBuffer(body))

	var req struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		log.Printf("extractModel: parsing body: %s", err)
		return ""
	}
	return req.Model
}

func getAPIKey(r *http.Request) string {
	key := r.Header.Get("Authorization")
	if key == "" && defaultAPIKey != "" {
		key = defaultAPIKey
	}
	return key
}

func newHandler(lp *llmProxy, rp *httputil.ReverseProxy) *http.ServeMux {
	serveMux := http.NewServeMux()

	serveMux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// stubbed locally, mirrors authproxy routes (never proxied to upstream)
		if isLookupPath(r.URL.Path) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`[]`))
			return
		}
		if isToolsPath(r.URL.Path) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"message":"this feature is disabled","type":"feature_disabled"}}`))
			return
		}

		if !isLLMPath(r.URL.Path) {
			rp.ServeHTTP(w, r)
			return
		}

		// Acquire the slot (mirrors llm.go's Before interceptor). The slot is
		// released by the ModifyResponse hook (body close -> grace) or the hard
		// cap, not by ServeHTTP returning.
		sq := lp.sq
		sq.Lock()
		var model string
		if r.Method == "POST" {
			model = extractModel(r)
			if model == "" {
				log.Printf("POST %s has no model field", r.URL.Path)
			}
		}
		apiKey := getAPIKey(r)
		if r.Method == "POST" {
			var queuedLogged bool
			sq.AwaitWithPredicateAndDescription(servicequeue.LLM, true, func() bool {
				// Re-enter only for the same user (key) that does not switch the
				// model; a different user is queued, not allowed to share the slot.
				canReent := lp.apiKey == apiKey && (model == "" || lp.model == "" || lp.model == model)
				// Log once when the request must wait, with neutral wording (this is
				// normal queuing behind another user, not an error), instead of on
				// every queue re-check, which previously spammed the log.
				if !canReent && !queuedLogged {
					queuedLogged = true
					log.Printf("queued behind holder: my key=%q held key=%q; my model=%q held model=%q",
						apiKey, lp.apiKey, model, lp.model)
				}
				return canReent
			}, model)
			lp.apiKey = apiKey
			lp.heldRequest++
			if lp.model != model {
				// Model switched (or first holder): start a fresh hold epoch.
				lp.heldSince = time.Now()
			}
			lp.model = model
		} else {
			sq.Await(servicequeue.LLM, false)
		}
		sq.CancelCleanup()
		sq.CF = &servicequeue.CleanupFunc{
			F: func() {
				lp.client.Get(lp.target.JoinPath("/unload").String())
			},
			Service: servicequeue.LLM,
		}
		// Arm the hard cap now, on slot acquisition. ModifyResponse re-arms it
		// (reset) when the upstream actually responds, preserving "cap from
		// response start" for live requests; but if the upstream hangs BEFORE
		// responding, ModifyResponse never runs and this is the only timer that
		// will free the slot, so a wedged connection no longer blocks everyone.
		sq.SetCleanup(hardCap)
		sq.Unlock()

		log.Printf("Serving: model=%s", model)

		rp.ServeHTTP(w, r)
	})

	serveMux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok")
	})
	return serveMux
}

func main() {
	if upstreamAddr == "" {
		log.Fatal("LLM_PROXY_UPSTREAM env var is required")
	}
	if listenAddr == "" {
		listenAddr = ":8081"
	}

	upstream, err := url.Parse(upstreamAddr)
	if err != nil {
		log.Fatalf("Invalid upstream URL: %s", err)
	}

	// The ServiceQueue broadcasts SvcUpdate to svcChan; we have no UI here, so
	// drain it to keep the sends from blocking.
	svcChan := make(chan servicequeue.SvcUpdate, 1000)
	go func() {
		for range svcChan {
		}
	}()
	sq := servicequeue.NewServiceQueue(svcChan)

	lp := &llmProxy{
		sq:     sq,
		target: upstream,
		client: http.Client{},
	}

	reverseProxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = upstream.Scheme
			req.URL.Host = upstream.Host
			delete(req.Header, "X-Forwarded-For")
			req.Header.Set("X-Forwarded-For", req.RemoteAddr)
		},
		// Mirrors llm.go's After interceptor: arm the hard-cap cleanup and wrap
		// the response body so the slot is released when the body closes (grace)
		// instead of when ServeHTTP returns.
		ModifyResponse: func(resp *http.Response) error {
			return sq.ServiceCloserWithAfterBody(servicequeue.LLM, func(path string) bool {
				return isLLMPath(path)
			}, hardCap, true, graceDelay)(resp.Request, resp)
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("Proxy error: %s", err)
			// The request failed without (or before) a response, so the
			// ModifyResponse body-close release never runs and the slot would be
			// held until the hard cap. Release it here the same way the body-close
			// does, so a failed request doesn't wedge the proxy for everyone.
			if r.Method == "POST" && isLLMPath(r.URL.Path) {
				sq.Lock()
				sq.CancelCleanup()
				d := graceDelay(r)
				if d > 0 {
					sq.SetService(servicequeue.WAIT)
					sq.SetCleanup(d)
				} else {
					sq.SetService(servicequeue.NONE)
				}
				sq.Unlock()
			}
			w.WriteHeader(http.StatusBadGateway)
			fmt.Fprintf(w, "Proxy error: %s\n", err)
		},
	}

	server := &http.Server{
		Addr:         listenAddr,
		Handler:      newHandler(lp, reverseProxy),
		ReadTimeout:  600 * time.Second,
		WriteTimeout: 600 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		log.Println("Shutting down...")
		server.Close()
	}()

	log.Printf("LLM Proxy listening on %s, upstream: %s", listenAddr, upstreamAddr)
	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatalf("Server error: %s", err)
	}
	log.Println("Server stopped")
}
