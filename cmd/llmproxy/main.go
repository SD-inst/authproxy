package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
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
	// vipGracePeriod is the (longer) hold applied to VIP keys. It falls back to
	// the normal gracePeriod when LLM_PROXY_VIP_GRACE_PERIOD is unset.
	vipGracePeriod = parseDuration(os.Getenv("LLM_PROXY_VIP_GRACE_PERIOD"), gracePeriod)
	// vipKeys holds the SHA-256 hashes of the VIP API keys, parsed from
	// LLM_PROXY_VIP_KEYS (comma-separated 64-hex-digit digests). Keys are stored
	// as hashes, never in cleartext.
	vipKeys = parseVipKeys(os.Getenv("LLM_PROXY_VIP_KEYS"))
	// hardCap is the safety net: it frees the slot even if a response body never
	// closes (stuck upstream connection). Mirrors llm.go's cleanup timeout.
	hardCap = parseDuration(os.Getenv("LLM_PROXY_HOLD_TIMEOUT"), 5*time.Minute)
	// modelRewrites transparently maps client-side model names to the names the
	// upstream is sent and the responses must carry back. Parsed from
	// LLM_PROXY_MODEL_REWRITES ("client=upstream" pairs, comma-separated): a
	// request that names a mapped model is rewritten to the upstream name
	// before it is proxied, and the upstream's answer is rewritten back to the
	// client name, so the client never sees the upstream's name. This lets one
	// upstream model stand in for another (e.g. a model that runs in a
	// separate container) without changing the name clients already use.
	modelRewrites = parseModelRewrites(os.Getenv("LLM_PROXY_MODEL_REWRITES"))
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

// parseVipKeys parses LLM_PROXY_VIP_KEYS (comma-separated 64-hex-digit SHA-256
// digests) into a set for O(1) lookup. Malformed entries are skipped with a log.
func parseVipKeys(s string) map[[32]byte]bool {
	m := make(map[[32]byte]bool)
	for _, part := range strings.Split(s, ",") {
		h := strings.TrimSpace(part)
		if h == "" {
			continue
		}
		b, err := hex.DecodeString(h)
		if err != nil || len(b) != 32 {
			log.Printf("Invalid VIP key hash %q (want 64-hex-digit SHA-256), skipping", h)
			continue
		}
		var k [32]byte
		copy(k[:], b)
		m[k] = true
	}
	return m
}

// parseModelRewrites parses LLM_PROXY_MODEL_REWRITES (comma-separated
// "client=upstream" pairs) into a map from the client-side model name to the
// name the upstream is sent. Malformed entries (no "=", or an empty side) are
// skipped with a log; a repeated client name keeps its last pair.
func parseModelRewrites(s string) map[string]string {
	m := make(map[string]string)
	for _, part := range strings.Split(s, ",") {
		p := strings.TrimSpace(part)
		if p == "" {
			continue
		}
		from, to, ok := strings.Cut(p, "=")
		from, to = strings.TrimSpace(from), strings.TrimSpace(to)
		if !ok || from == "" || to == "" {
			log.Printf("Invalid model rewrite %q (want client=upstream), skipping", p)
			continue
		}
		m[from] = to
	}
	return m
}

// apiKeyVariants returns the forms of an Authorization header value a key can
// be configured by: the bare key (the documented form — the optional "Bearer "
// scheme is stripped) and, when the header carried a scheme, the header value
// as the client sent it. An empty header has no variants.
func apiKeyVariants(header string) []string {
	header = strings.TrimSpace(header)
	if header == "" {
		return nil
	}
	bare := header
	// The scheme is not part of the key (HTTP auth schemes are case-insensitive),
	// so the bare key is a variant of its own.
	if scheme, rest, ok := strings.Cut(header, " "); ok && strings.EqualFold(scheme, "Bearer") {
		bare = strings.TrimSpace(rest)
	}
	variants := []string{bare}
	if bare != header {
		variants = append(variants, header)
	}
	return variants
}

// isVIPKey reports whether the API key is a configured VIP key, matched by its
// SHA-256 hash against vipKeys. The argument is the Authorization header value:
// both the bare key (the documented form, "Bearer " scheme stripped) and the
// header as the client sent it are hashed, so a digest of either matches.
func isVIPKey(authorization string) bool {
	for _, variant := range apiKeyVariants(authorization) {
		if vipKeys[sha256.Sum256([]byte(variant))] {
			return true
		}
	}
	return false
}

// llmProxy holds the shared state. apiKey/model are guarded by the ServiceQueue
// mutex (they are only touched while sq is locked).
type llmProxy struct {
	sq     *servicequeue.ServiceQueue
	target *url.URL
	client http.Client
	apiKey string
	model  string
}

// releaseKey carries the slot releaser of the request that acquired the slot
// through the request context, so that both release sites — the response-body
// close hook and the proxy error handler — act on the holder of their own
// request instead of guessing it from the method.
type releaseKey struct{}

// slotRelease pairs with the Hold() taken when the request acquired the slot.
// It is idempotent: the body-close hook and the error handler can both fire for
// one request, and the slot must be freed at most once per holder.
type slotRelease struct {
	sq   *servicequeue.ServiceQueue
	once sync.Once
}

func (s *slotRelease) release(r *http.Request) {
	s.once.Do(func() {
		s.sq.Lock()
		defer s.sq.Unlock()
		s.sq.Release(hardCap, graceDelay(r))
	})
}

// modelRewriteKey carries the client-side model name of an aliased request
// (see modelRewrites) through the request context: the request body is consumed
// and rewritten before the proxy sends it, so the response path recovers the
// original name from here to rewrite the answer back.
type modelRewriteKey struct{}

// releaserFor returns the releaser attached to a proxied request, or nil when
// the request never entered the queue (not an LLM path, or a locally stubbed
// route) and therefore has no slot to free.
func releaserFor(r *http.Request) *slotRelease {
	if r == nil {
		return nil
	}
	rel, _ := r.Context().Value(releaseKey{}).(*slotRelease)
	return rel
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

// graceDelay mirrors llm.go's waitAfterBody. VIP keys hold their model for the
// longer vipGracePeriod; all other keys use the default gracePeriod.
func graceDelay(r *http.Request) time.Duration {
	if r.Method == "POST" &&
		(strings.Contains(r.URL.Path, "/completions") || strings.Contains(r.URL.Path, "/embeddings")) {
		if isVIPKey(getAPIKey(r)) {
			return vipGracePeriod
		}
		return gracePeriod
	}
	return 0
}

// pathModel returns the model named by a /upstream/:model/... path (up to the
// first /v1/ segment), or "" for any other path.
func pathModel(path string) string {
	if m, ok := strings.CutPrefix(path, "/upstream/"); ok {
		if idx := strings.Index(m, "/v1/"); idx >= 0 {
			m = m[:idx]
		}
		return m
	}
	return ""
}

// extractModel returns the model a request names and the request's raw body.
// The model comes from the /upstream/:model/... path prefix when present (the
// body is left untouched and the body result is nil), otherwise from the
// "model" field of the JSON body. The body branch consumes the request body,
// so the caller must re-attach the returned bytes to the request before it is
// served.
func extractModel(r *http.Request) (model string, body []byte) {
	if m := pathModel(r.URL.Path); m != "" {
		return m, nil
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Printf("extractModel: reading body: %s", err)
		return "", body
	}

	var req struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		log.Printf("extractModel: parsing body: %s", err)
		return "", body
	}
	return req.Model, body
}

// rewriteModelInBody replaces the "model" field of a JSON object body that
// equals from with to, reporting whether anything changed. The body is
// returned unchanged when it is not a JSON object, has no "model" field, or
// the field does not equal from; all other fields pass through byte-for-byte.
func rewriteModelInBody(body []byte, from, to string) ([]byte, bool) {
	if from == "" || from == to {
		return body, false
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return body, false
	}
	raw, ok := obj["model"]
	if !ok {
		return body, false
	}
	var cur string
	if err := json.Unmarshal(raw, &cur); err != nil || cur != from {
		return body, false
	}
	renamed, err := json.Marshal(to)
	if err != nil {
		return body, false
	}
	obj["model"] = renamed
	out, err := json.Marshal(obj)
	if err != nil {
		return body, false
	}
	return out, true
}

func getAPIKey(r *http.Request) string {
	key := r.Header.Get("Authorization")
	if key == "" && defaultAPIKey != "" {
		key = defaultAPIKey
	}
	return key
}

// forceCORS replaces any Access-Control-Allow-Origin headers with exactly one
// holding "*". llama.cpp emits a wrong value and may repeat the header several
// times, and a duplicated header is invalid for browsers (they only honour a
// single value), so Del-then-Set is required rather than Set alone.
func forceCORS(h http.Header) {
	h.Del("Access-Control-Allow-Origin")
	h.Set("Access-Control-Allow-Origin", "*")
}

// rewriteResponseModel makes an aliased request's answer carry the client-side
// model name again: the upstream responded with the name it was sent, and the
// client must not see it. A JSON body is re-encoded whole (its Content-Length
// is corrected to match); an SSE stream is rewritten line by line as it flows,
// so the response stays live end to end. A response whose model field is
// absent or does not match the upstream name passes through unchanged.
func rewriteResponseModel(resp *http.Response, from, to string) {
	ct := resp.Header.Get("Content-Type")
	switch {
	case strings.HasPrefix(ct, "text/event-stream"):
		resp.Body = &sseModelRewriteBody{orig: resp.Body, from: from, to: to}
	case strings.HasPrefix(ct, "application/json"):
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close() // the upstream response is fully received; its connection is released
		body, _ = rewriteModelInBody(body, from, to)
		resp.Body = io.NopCloser(bytes.NewReader(body))
		resp.ContentLength = int64(len(body))
		resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
	}
}

// sseModelRewriteBody streams an SSE response, replacing the "model" field of
// each "data:" line that carries the upstream name with the client's name.
// It reads line by line, so the stream stays live: at any moment only the
// current event is in flight, and a client disconnect propagates to the
// upstream through the original body's Read/Close.
type sseModelRewriteBody struct {
	orig io.ReadCloser
	from string
	to   string

	sc   *bufio.Scanner
	buf  bytes.Buffer
	done bool
	err  error
}

func (m *sseModelRewriteBody) Close() error {
	return m.orig.Close()
}

func (m *sseModelRewriteBody) Read(p []byte) (int, error) {
	if m.sc == nil {
		m.sc = bufio.NewScanner(m.orig)
		m.sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	}
	for m.buf.Len() == 0 {
		if m.done {
			return 0, m.err
		}
		if !m.sc.Scan() {
			m.done = true
			m.err = m.sc.Err() // nil on clean EOF
			if m.err == nil {
				m.err = io.EOF
			}
			return 0, m.err
		}
		m.buf.Write(m.rewriteLine(m.sc.Bytes()))
		m.buf.WriteByte('\n')
	}
	return m.buf.Read(p)
}

// rewriteLine rewrites one SSE line in place, returning it unchanged unless it
// is a "data:" line whose JSON payload carries a model field equal to from.
func (m *sseModelRewriteBody) rewriteLine(line []byte) []byte {
	const prefix = "data:"
	if !bytes.HasPrefix(line, []byte(prefix)) {
		return line
	}
	payload := bytes.TrimSpace(line[len(prefix):])
	if len(payload) == 0 || payload[0] != '{' {
		return line // "data: [DONE]", comments, non-JSON
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(payload, &obj); err != nil {
		return line
	}
	raw, ok := obj["model"]
	if !ok {
		return line
	}
	var cur string
	if err := json.Unmarshal(raw, &cur); err != nil || cur != m.from {
		return line
	}
	renamed, err := json.Marshal(m.to)
	if err != nil {
		return line
	}
	obj["model"] = renamed
	out, err := json.Marshal(obj)
	if err != nil {
		return line
	}
	return append([]byte(prefix+" "), out...)
}

func newHandler(lp *llmProxy, rp *httputil.ReverseProxy) *http.ServeMux {
	serveMux := http.NewServeMux()

	serveMux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// A CORS preflight is a browser handshake, not a model request, so it
		// must never enter the queue: while the slot is busy the browser sits on
		// a preflight that cannot complete until the slot is freed, and the real
		// request behind it hangs with it. Answered for every path, because the
		// upstream's own preflight answer lacks the CORS headers anyway.
		if r.Method == http.MethodOptions {
			forceCORS(w.Header())
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			if reqHeaders := r.Header.Get("Access-Control-Request-Headers"); reqHeaders != "" {
				w.Header().Set("Access-Control-Allow-Headers", reqHeaders)
			} else {
				w.Header().Set("Access-Control-Allow-Headers", "*")
			}
			w.Header().Set("Access-Control-Max-Age", "86400")
			w.WriteHeader(http.StatusNoContent)
			return
		}

		// lookup and tools are stubbed locally, mirroring authproxy routes
		// (never proxied to upstream)
		if isLookupPath(r.URL.Path) {
			forceCORS(w.Header())
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`[]`))
			return
		}
		if isToolsPath(r.URL.Path) {
			forceCORS(w.Header())
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
		// released by the ModifyResponse hook (body close -> grace) or by the
		// proxy error handler, not by ServeHTTP returning.
		sq := lp.sq
		sq.Lock()
		var (
			model   string
			rawBody []byte
		)
		if r.Method == "POST" {
			model, rawBody = extractModel(r)
			if model == "" {
				log.Printf("POST %s has no model field", r.URL.Path)
			}
		}
		apiKey := getAPIKey(r)

		// LLM_PROXY_MODEL_REWRITES transparently maps a client-side model name
		// to the name the upstream is sent: a request that names a mapped model
		// is rewritten to the upstream name before it is proxied, and the
		// upstream's answer is rewritten back to the client name (see
		// rewriteResponseModel), so the client never sees the upstream's name.
		// Only body-named models are aliased: a /upstream/:model/ path names the
		// model in the URL, which this proxy does not re-route.
		outModel := model
		if model != "" && pathModel(r.URL.Path) == "" {
			if to, ok := modelRewrites[model]; ok {
				outModel = to
				r = r.WithContext(context.WithValue(r.Context(), modelRewriteKey{}, model))
				if newBody, changed := rewriteModelInBody(rawBody, model, to); changed {
					rawBody = newBody
				}
			}
		}
		if rawBody != nil {
			// The body was consumed by extractModel; re-attach it (possibly
			// rewritten) so the proxy can send it to the upstream. A definite
			// Content-Length also normalizes requests that arrived chunked.
			r.Body = io.NopCloser(bytes.NewBuffer(rawBody))
			r.ContentLength = int64(len(rawBody))
		}

		if r.Method == "POST" {
			var queuedLogged bool
			sq.AwaitWithPredicateAndDescription(servicequeue.LLM, true, func() bool {
				// Re-enter only when the requested model matches the held one
				// (or either side is unknown); a different model is queued
				// rather than sharing the slot. The comparison is on the
				// upstream (effective) name, so aliases of one real model share
				// its slot, and a client that names the real name directly
				// shares it too.
				canReent := outModel == "" || lp.model == "" || lp.model == outModel
				// A VIP key holds its model exclusively: a non-VIP key may not
				// intercept (re-enter) the slot while the current holder is VIP, so
				// it stays queued until the VIP's grace period expires and the slot
				// is freed. A VIP re-entering its own model is unaffected.
				if canReent && !isVIPKey(apiKey) && isVIPKey(lp.apiKey) {
					canReent = false
				}
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
			// A request that names no model cannot change which model is held, so
			// it must not overwrite the held model with "": an empty held model
			// makes the re-entry predicate match every other model, letting an
			// unrelated request interrupt a generation already in flight. The held
			// model is the upstream name: it is what the upstream actually loads.
			if outModel != "" {
				lp.model = outModel
			}
		} else {
			sq.Await(servicequeue.LLM, false)
		}
		sq.Hold()
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
		// Bind the release to this request: the same holder is freed whichever
		// of the two paths runs — the response body closing, or a proxy error
		// before/without a response — and it is freed exactly once.
		rel := &slotRelease{sq: sq}
		r = r.WithContext(context.WithValue(r.Context(), releaseKey{}, rel))
		sq.Unlock()

		if outModel != model {
			log.Printf("Serving: model=%s (upstream: %s)", model, outModel)
		} else {
			log.Printf("Serving: model=%s", model)
		}

		rp.ServeHTTP(w, r)
	})

	serveMux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		forceCORS(w.Header())
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok")
	})
	return serveMux
}

// newReverseProxy assembles the upstream-facing ReverseProxy (shared by main
// and the tests, so the tests exercise the production wiring): the targeting
// director, the CORS fix, the response-side model rewrite for aliased
// requests, and the queue release wiring.
func newReverseProxy(lp *llmProxy, upstream *url.URL) *httputil.ReverseProxy {
	sq := lp.sq
	return &httputil.ReverseProxy{
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
			// Runs before the upstream headers are copied to the client, so this
			// is where duplicated upstream values are collapsed into one.
			forceCORS(resp.Header)
			rel := releaserFor(resp.Request)
			if rel == nil {
				// A request that never entered the queue has no slot to keep or
				// to free (this covers every non-LLM path).
				return nil
			}
			// An aliased request (LLM_PROXY_MODEL_REWRITES) must see its own
			// name in the answer: the upstream responded with the name it was
			// sent, and the client must not see it. Runs before the release
			// wrapping, which re-wraps whatever resp.Body holds — so a rewritten
			// body (re-encoded JSON or the live SSE transformer) is the one
			// whose close releases the slot.
			if clientName, ok := resp.Request.Context().Value(modelRewriteKey{}).(string); ok {
				if upstreamName, ok := modelRewrites[clientName]; ok {
					rewriteResponseModel(resp, upstreamName, clientName)
				}
			}
			return sq.ServiceCloserWithRelease(servicequeue.LLM, hardCap, func() {
				rel.release(resp.Request)
			})(resp.Request, resp)
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("Proxy error: %s", err)
			// The request failed without (or before) a response, so the
			// body-close release never runs. Free the holder of this very
			// request, whatever its method: Hold is taken for every method that
			// enters the queue, so the release must not be method-dependent.
			if rel := releaserFor(r); rel != nil {
				rel.release(r)
			}
			forceCORS(w.Header())
			w.WriteHeader(http.StatusBadGateway)
			fmt.Fprintf(w, "Proxy error: %s\n", err)
		},
	}
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

	reverseProxy := newReverseProxy(lp, upstream)

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
	if len(vipKeys) > 0 {
		log.Printf("VIP keys configured: %d, VIP grace period: %s", len(vipKeys), vipGracePeriod)
	}
	if len(modelRewrites) > 0 {
		pairs := make([]string, 0, len(modelRewrites))
		for from, to := range modelRewrites {
			pairs = append(pairs, from+"="+to)
		}
		sort.Strings(pairs)
		log.Printf("Model rewrites: %s", strings.Join(pairs, ", "))
	}
	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatalf("Server error: %s", err)
	}
	log.Println("Server stopped")
}
