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
	holdTimeout   = parseDuration(os.Getenv("LLM_PROXY_HOLD_TIMEOUT"), 120*time.Second)
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
	inFlight      int // number of active requests in the current epoch
	epoch         int // bumped whenever the slot is freed; stale requestDone becomes a no-op
	releaseTimer  *time.Timer // grace timer, armed after the last request finishes
	releaseGen    int
	holdTimer     *time.Timer // hard cap, frees the slot even if an upstream connection is stuck
	holdGen       int
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

// isLookupPath matches the authproxy route /upstream/:model/v1/streams/lookup.
// Any method: the web UI polls it, and the original GET-only stub leaked POST traffic.
func isLookupPath(path string) bool {
	return strings.HasPrefix(path, "/upstream/") &&
		strings.HasSuffix(path, "/v1/streams/lookup")
}

// isToolsPath matches the authproxy route /upstream/:model/tools.
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

// acquire blocks until this request may proceed and returns the epoch of the
// held slot this request joined.
// Re-enters if the same api key is active (and the held model is empty or matches).
// Cancels the release timer so the grace period resets on every new request,
// and re-arms the hold cap so a stuck upstream connection cannot hold the slot forever.
func (ps *proxyState) acquire(model, apiKey string) int {
	ps.mu.Lock()
	defer ps.mu.Unlock()

	for {
		if !ps.busy {
			ps.busy = true
			ps.epoch++
			ps.currentModel = model
			ps.currentAPIKey = apiKey
			ps.inFlight = 1
			ps.resetHoldTimerLocked()
			return ps.epoch
		}
		// mirrors llm.go predicate: apiKey == prevKey && (result.model == "" || prevModel == model)
		// a request without a model cannot switch the loaded one, so it joins the current holder
		if apiKey == ps.currentAPIKey && (model == "" || ps.currentModel == "" || ps.currentModel == model) {
			ps.stopReleaseTimerLocked()
			ps.inFlight++
			ps.currentModel = model // mirrors result.model = model; may be ""
			ps.resetHoldTimerLocked()
			return ps.epoch
		}
		log.Printf("Queueing: model=%s key=%s, held model=%s key=%s",
			model, redactKey(apiKey), ps.currentModel, redactKey(ps.currentAPIKey))
		ps.cv.Wait()
	}
}

// requestDone is called after the response body has been fully sent.
// The release is only scheduled by the last request to finish: with a zero
// delay it releases immediately, otherwise a grace timer is armed.
// A request whose epoch is stale (the hold cap already freed the slot) is a no-op.
func (ps *proxyState) requestDone(epoch int, delay time.Duration) {
	ps.mu.Lock()
	defer ps.mu.Unlock()

	if epoch != ps.epoch {
		return
	}
	if ps.inFlight <= 0 {
		return
	}
	ps.inFlight--
	if ps.inFlight > 0 {
		return
	}

	ps.stopHoldTimerLocked()
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

// releaseLocked clears the held model/key, invalidates the epoch (so any still
// finishing request of the old epoch becomes a no-op) and wakes waiters.
// Callers must hold ps.mu.
func (ps *proxyState) releaseLocked() {
	ps.busy = false
	ps.epoch++
	ps.inFlight = 0
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

// resetHoldTimerLocked arms the hard cap: after holdTimeout the slot is freed
// even if in-flight requests never finish (stuck/lingering upstream connection).
// Callers must hold ps.mu.
func (ps *proxyState) resetHoldTimerLocked() {
	ps.stopHoldTimerLocked()
	ps.holdGen++
	gen := ps.holdGen
	ps.holdTimer = time.AfterFunc(holdTimeout, func() {
		ps.mu.Lock()
		defer ps.mu.Unlock()
		if ps.holdGen != gen || !ps.busy {
			return
		}
		log.Printf("Hold timeout: releasing stuck slot, model=%s key=%s",
			ps.currentModel, redactKey(ps.currentAPIKey))
		ps.stopHoldTimerLocked()
		ps.stopReleaseTimerLocked()
		ps.releaseLocked()
	})
}

// stopHoldTimerLocked cancels an armed hold-cap timer. Callers must hold ps.mu.
func (ps *proxyState) stopHoldTimerLocked() {
	if ps.holdTimer != nil {
		ps.holdTimer.Stop()
		ps.holdTimer = nil
	}
	ps.holdGen++
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

		var model string
		if r.Method == "POST" {
			model = extractModel(r)
			if model == "" {
				log.Printf("POST %s has no model field", r.URL.Path)
			}
		}
		apiKey := getAPIKey(r)

		epoch := ps.acquire(model, apiKey)

		log.Printf("Serving: model=%s", model)

		// ServeHTTP blocks until the upstream body (incl. streaming) is fully sent,
		// so releasing here is after the response completes. If the connection
		// lingers longer than holdTimeout, the hold cap frees the slot first and
		// this requestDone becomes a no-op (stale epoch).
		rp.ServeHTTP(w, r)

		ps.requestDone(epoch, graceDelay(r))
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
