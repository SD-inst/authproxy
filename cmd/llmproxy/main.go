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
	"sync"
	"syscall"
	"time"
)

var (
	upstreamAddr  = os.Getenv("LLM_PROXY_UPSTREAM")
	listenAddr    = os.Getenv("LLM_PROXY_LISTEN")
	defaultAPIKey = os.Getenv("LLM_PROXY_DEFAULT_API_KEY")
	gracePeriod   = parseDuration(os.Getenv("LLM_PROXY_GRACE_PERIOD"), 10*time.Second)
)

func parseDuration(s string, def time.Duration) time.Duration {
	if s == "" {
		return def
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		log.Printf("Invalid LLM_PROXY_GRACE_PERIOD %q, using %s", s, def)
		return def
	}
	return d
}

type proxyState struct {
	mu            sync.Mutex
	cv            *sync.Cond
	busy          bool
	currentModel  string
	currentAPIKey string
	inFlight      int // number of in-flight blocking requests
	releaseTimer  *time.Timer
	releaseGen    int
}

func newProxyState() *proxyState {
	ps := &proxyState{}
	ps.cv = sync.NewCond(&ps.mu)
	return ps
}

// isLLMPath mirrors llm.go's isLLMPath
func isLLMPath(path string) bool {
	return (strings.HasSuffix(path, "/v1/chat/completions") || strings.HasSuffix(path, "/v1/completions") ||
		strings.HasSuffix(path, "/v1/internal/encode") || strings.HasSuffix(path, "/v1/embeddings") ||
		strings.HasPrefix(path, "/upstream/")) && !strings.HasSuffix(path, ".js")
}

// isLookupPath matches the authproxy route POST /upstream/:model/v1/streams/lookup
func isLookupPath(method, path string) bool {
	return method == http.MethodPost &&
		strings.HasPrefix(path, "/upstream/") &&
		strings.HasSuffix(path, "/v1/streams/lookup")
}

// isToolsPath matches the authproxy route GET /upstream/:model/tools
func isToolsPath(method, path string) bool {
	return method == http.MethodGet &&
		strings.HasPrefix(path, "/upstream/") &&
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

// acquire blocks until this request may proceed.
// Re-enters if the same api key is active (and the held model is empty or matches).
// Cancels the release timer so the grace period resets on every new request.
func (ps *proxyState) acquire(model, apiKey string) {
	ps.mu.Lock()
	defer ps.mu.Unlock()

	for {
		if !ps.busy {
			ps.busy = true
			ps.currentModel = model
			ps.currentAPIKey = apiKey
			ps.inFlight = 1
			return
		}
		// mirrors llm.go predicate: apiKey == prevKey && (result.model == "" || prevModel == model)
		// a request without a model cannot switch the loaded one, so it joins the current holder
		if apiKey == ps.currentAPIKey && (model == "" || ps.currentModel == "" || ps.currentModel == model) {
			ps.stopReleaseTimerLocked()
			ps.inFlight++
			ps.currentModel = model // mirrors result.model = model; may be ""
			return
		}
		log.Printf("Queueing: model=%s key=%s, held model=%s key=%s",
			model, redactKey(apiKey), ps.currentModel, redactKey(ps.currentAPIKey))
		ps.cv.Wait()
	}
}

// requestDone is called after the response body has been fully sent.
// The release is only scheduled by the last request to finish: with a zero
// delay it releases immediately, otherwise a grace timer is armed.
func (ps *proxyState) requestDone(delay time.Duration) {
	ps.mu.Lock()
	defer ps.mu.Unlock()

	if ps.inFlight <= 0 {
		return
	}
	ps.inFlight--
	if ps.inFlight > 0 {
		return
	}

	ps.stopReleaseTimerLocked()
	if delay <= 0 {
		ps.releaseLocked()
		return
	}
	ps.releaseGen++
	gen := ps.releaseGen
	ps.releaseTimer = time.AfterFunc(delay, func() {
		ps.mu.Lock()
		defer ps.mu.Unlock()
		if ps.releaseGen != gen || !ps.busy || ps.inFlight != 0 {
			return
		}
		ps.releaseTimer = nil
		ps.releaseLocked()
	})
}

// releaseLocked clears the held model/key and wakes waiters. Callers must hold ps.mu.
func (ps *proxyState) releaseLocked() {
	ps.busy = false
	ps.currentModel = ""
	ps.currentAPIKey = ""
	ps.cv.Broadcast()
}

// stopReleaseTimerLocked cancels an armed release timer. Callers must hold ps.mu.
func (ps *proxyState) stopReleaseTimerLocked() {
	if ps.releaseTimer != nil {
		ps.releaseTimer.Stop()
		ps.releaseTimer = nil
	}
	ps.releaseGen++
}

func redactKey(key string) string {
	if len(key) <= 8 {
		return "***"
	}
	return key[:8] + "..."
}

func newHandler(ps *proxyState, rp *httputil.ReverseProxy) *http.ServeMux {
	serveMux := http.NewServeMux()

	serveMux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// stubbed locally, mirrors authproxy routes (never proxied to upstream)
		if isLookupPath(r.Method, r.URL.Path) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`[]`))
			return
		}
		if isToolsPath(r.Method, r.URL.Path) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"message":"this feature is disabled","type":"feature_disabled"}}`))
			return
		}

		if !isLLMPath(r.URL.Path) {
			rp.ServeHTTP(w, r)
			return
		}

		var model string
		if r.Method == "POST" {
			model = extractModel(r)
			if model == "" {
				log.Printf("POST %s has no model field", r.URL.Path)
			}
		}
		apiKey := getAPIKey(r)

		ps.acquire(model, apiKey)

		log.Printf("Serving: model=%s", model)

		// ServeHTTP blocks until the upstream body (incl. streaming) is fully sent,
		// so releasing here is after the response completes.
		rp.ServeHTTP(w, r)

		ps.requestDone(graceDelay(r))
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

	ps := newProxyState()

	reverseProxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = upstream.Scheme
			req.URL.Host = upstream.Host
			delete(req.Header, "X-Forwarded-For")
			req.Header.Set("X-Forwarded-For", req.RemoteAddr)
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("Proxy error: %s", err)
			w.WriteHeader(http.StatusBadGateway)
			fmt.Fprintf(w, "Proxy error: %s\n", err)
		},
	}

	server := &http.Server{
		Addr:         listenAddr,
		Handler:      newHandler(ps, reverseProxy),
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
