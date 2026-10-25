package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rkfg/authproxy/servicequeue"
)

func newFakeUpstream(t *testing.T, sleep time.Duration) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/completions") || strings.HasSuffix(r.URL.Path, "/embeddings") ||
			strings.HasSuffix(r.URL.Path, "/encode") {
			time.Sleep(sleep)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	}))
}

func newTestProxy(upstream *url.URL) *httptest.Server {
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
	// The production ReverseProxy, so the tests exercise the real wiring
	// (including the response-side model rewrite and CORS fix).
	return httptest.NewServer(newHandler(lp, newReverseProxy(lp, upstream)))
}

func doRequest(t *testing.T, proxyURL, path, model, key string) (start, end time.Time, err error) {
	t.Helper()
	body := `{"prompt":"x"}`
	if model != "" {
		body = fmt.Sprintf(`{"model":%q,"prompt":"x"}`, model)
	}
	req, err := http.NewRequest("POST", proxyURL+path, strings.NewReader(body))
	if err != nil {
		return time.Now(), time.Now(), err
	}
	req.Header.Set("Authorization", key)
	start = time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return start, time.Now(), err
	}
	defer resp.Body.Close()
	if _, err := io.ReadAll(resp.Body); err != nil {
		return start, time.Now(), err
	}
	end = time.Now()
	return start, end, nil
}

// startCancelable starts a request with a cancelable context. It returns a
// cancel function and a channel that receives the finish time (when the request
// completes or is canceled).
func startCancelable(t *testing.T, proxyURL, path, model, key string) (cancel func(), done <-chan time.Time) {
	t.Helper()
	ch := make(chan time.Time, 1)
	body := `{"prompt":"x"}`
	if model != "" {
		body = fmt.Sprintf(`{"model":%q,"prompt":"x"}`, model)
	}
	req, err := http.NewRequest("POST", proxyURL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	req.Header.Set("Authorization", key)
	ctx, cancelFn := context.WithCancel(req.Context())
	req = req.WithContext(ctx)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_, _ = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
		}
		ch <- time.Now()
	}()
	return cancelFn, ch
}

// A1 and A2 (same user) run concurrently: A2 re-enters the queue without waiting.
// B (different model/key) must stay queued until after the last A finishes + grace period.
func TestGracePeriodBlocksDifferentModel(t *testing.T) {
	const sleep = 400 * time.Millisecond
	const grace = 300 * time.Millisecond

	upstream := newFakeUpstream(t, sleep)
	defer upstream.Close()
	upURL, _ := url.Parse(upstream.URL)

	oldGrace := gracePeriod
	gracePeriod = grace
	defer func() { gracePeriod = oldGrace }()

	proxy := newTestProxy(upURL)
	defer proxy.Close()

	var (
		wg       sync.WaitGroup
		a1s, a1e time.Time
		a2s, a2e time.Time
		bs, be   time.Time
	)
	wg.Add(2)

	// A2 is a re-entry: same model and key as A1, started while A1 is in flight.
	// Wait so A1 acquires the slot first, otherwise A2 would queue behind A1
	// and the "re-enter immediately" assertion below becomes timing-dependent.
	go func() {
		defer wg.Done()
		time.Sleep(100 * time.Millisecond)
		a2s, a2e, _ = doRequest(t, proxy.URL, "/v1/chat/completions", "llama", "keyA")
	}()
	go func() {
		defer wg.Done()
		// start after A1 and A2 have acquired the queue
		time.Sleep(100 * time.Millisecond)
		bs, be, _ = doRequest(t, proxy.URL, "/v1/chat/completions", "mistral", "keyB")
	}()
	a1s, a1e, err := doRequest(t, proxy.URL, "/v1/chat/completions", "llama", "keyA")
	if err != nil {
		t.Fatalf("A1 failed: %v", err)
	}
	wg.Wait()

	if a2e.Sub(a2s) > 2*sleep {
		t.Errorf("A2 should re-enter immediately, took %v (expected < %v)", a2e.Sub(a2s), 2*sleep)
	}
	if a1e.Sub(a1s) > 2*sleep {
		t.Errorf("A1 was unexpectedly blocked, took %v", a1e.Sub(a1s))
	}
	// B must not be released before the later of A1/A2 finished + grace period.
	lastA := a1e
	if a2e.After(lastA) {
		lastA = a2e
	}
	earliestB := lastA.Add(grace)
	if be.Before(earliestB.Add(-100 * time.Millisecond)) {
		t.Errorf("B released too early: finished %v, but grace ends at %v", be, earliestB)
	}
	// B's own duration should be ~sleep, not queued time.
	if be.Sub(bs) > sleep+2*time.Second {
		t.Errorf("B took too long: %v", be.Sub(bs))
	}
}

// A1 (completions, grace) and A2 (encode, no grace) overlap; A2 finishes last.
// Release happens right after the last request finishes (encode has no grace),
// and B must not be released before A2 finishes.
func TestReleaseAfterLastRequest(t *testing.T) {
	const sleep = 400 * time.Millisecond
	const grace = 500 * time.Millisecond

	upstream := newFakeUpstream(t, sleep)
	defer upstream.Close()
	upURL, _ := url.Parse(upstream.URL)

	oldGrace := gracePeriod
	gracePeriod = grace
	defer func() { gracePeriod = oldGrace }()

	proxy := newTestProxy(upURL)
	defer proxy.Close()

	var (
		wg     sync.WaitGroup
		a2e    time.Time
		bs, be time.Time
	)
	wg.Add(2)

	// A2 (encode) is slower than A1 (completions) and finishes last.
	go func() {
		defer wg.Done()
		_, a2e, _ = doRequest(t, proxy.URL, "/v1/internal/encode", "llama", "keyA")
	}()
	go func() {
		defer wg.Done()
		// start after A1 and A2 have acquired the queue
		time.Sleep(100 * time.Millisecond)
		bs, be, _ = doRequest(t, proxy.URL, "/v1/chat/completions", "mistral", "keyB")
	}()
	_, _, err := doRequest(t, proxy.URL, "/v1/chat/completions", "llama", "keyA")
	if err != nil {
		t.Fatalf("A1 failed: %v", err)
	}
	wg.Wait()

	// Encode has no grace: B can only start after A2 (the last A) finished.
	if be.Before(a2e.Add(-100 * time.Millisecond)) {
		t.Errorf("B finished before A2: be=%v a2e=%v", be, a2e)
	}
	if be.Sub(bs) > sleep+2*time.Second {
		t.Errorf("B took too long: %v", be.Sub(bs))
	}
}

// A request without a "model" field (same key) cannot switch the loaded model,
// so it must re-enter the queue instead of waiting for the held model to release.
// It must also leave the held model recorded, otherwise the next request with a
// different model sees "no model held" and barges into a generation in flight.
func TestNoModelRequestReenters(t *testing.T) {
	const sleep = 400 * time.Millisecond
	const grace = 300 * time.Millisecond

	upstream := newFakeUpstream(t, sleep)
	defer upstream.Close()
	upURL, _ := url.Parse(upstream.URL)

	oldGrace := gracePeriod
	gracePeriod = grace
	defer func() { gracePeriod = oldGrace }()

	proxy := newTestProxy(upURL)
	defer proxy.Close()

	var (
		wg       sync.WaitGroup
		a1s, a1e time.Time
		a2s, a2e time.Time
		bs, be   time.Time
	)
	wg.Add(2)

	// A1 holds the model "llama"; A2 (same key, no model) must re-enter immediately.
	// A2 is started only after A1 has acquired, so the test exercises re-entry
	// into a held model rather than racing over who acquires first.
	go func() {
		defer wg.Done()
		time.Sleep(100 * time.Millisecond)
		a2s, a2e, _ = doRequest(t, proxy.URL, "/v1/chat/completions", "", "keyA")
	}()
	go func() {
		defer wg.Done()
		time.Sleep(100 * time.Millisecond)
		bs, be, _ = doRequest(t, proxy.URL, "/v1/chat/completions", "mistral", "keyB")
	}()
	a1s, a1e, err := doRequest(t, proxy.URL, "/v1/chat/completions", "llama", "keyA")
	if err != nil {
		t.Fatalf("A1 failed: %v", err)
	}
	wg.Wait()

	// A2 must not wait for the grace period.
	if a2e.Sub(a2s) > 2*sleep {
		t.Errorf("model-less request should re-enter, took %v (expected < %v)", a2e.Sub(a2s), 2*sleep)
	}
	if a1e.Sub(a1s) > 2*sleep {
		t.Errorf("A1 was unexpectedly blocked, took %v", a1e.Sub(a1s))
	}
	// B still waits for the last A + grace.
	lastA := a1e
	if a2e.After(lastA) {
		lastA = a2e
	}
	if be.Before(lastA.Add(grace - 100*time.Millisecond)) {
		t.Errorf("B released too early: %v, grace ends at %v", be, lastA.Add(grace))
	}
	// B should not be double-blocked; total time bounded by queue + one upstream call.
	if be.Sub(bs) > 2*sleep+grace+2*time.Second {
		t.Errorf("B took too long: %v", be.Sub(bs))
	}
}

// After the grace period expires the queue frees; the next request (any model)
// proceeds immediately.
func TestQueueFreesAfterGrace(t *testing.T) {
	const sleep = 300 * time.Millisecond
	const grace = 300 * time.Millisecond

	upstream := newFakeUpstream(t, sleep)
	defer upstream.Close()
	upURL, _ := url.Parse(upstream.URL)

	oldGrace := gracePeriod
	gracePeriod = grace
	defer func() { gracePeriod = oldGrace }()

	proxy := newTestProxy(upURL)
	defer proxy.Close()

	s, e, err := doRequest(t, proxy.URL, "/v1/chat/completions", "llama", "keyA")
	if err != nil {
		t.Fatalf("first request failed: %v", err)
	}
	_ = s
	_ = e
	// wait for grace to elapse
	time.Sleep(grace + 50*time.Millisecond)

	cs, ce, err := doRequest(t, proxy.URL, "/v1/chat/completions", "mistral", "keyB")
	if err != nil {
		t.Fatalf("second request failed: %v", err)
	}
	if ce.Sub(cs) > 2*sleep {
		t.Errorf("second request should pass immediately, took %v", ce.Sub(cs))
	}
}

// Three parallel requests from user A hold the slot (re-enter). Two are canceled
// client-side; the slot must stay held while the third is still in flight, so a
// different user B is queued until the last A finishes + grace.
func TestCanceledRequestsDontFreeSlotEarly(t *testing.T) {
	const sleep = 600 * time.Millisecond
	const grace = 300 * time.Millisecond

	upstream := newFakeUpstream(t, sleep)
	defer upstream.Close()
	upURL, _ := url.Parse(upstream.URL)

	oldGrace := gracePeriod
	gracePeriod = grace
	defer func() { gracePeriod = oldGrace }()

	proxy := newTestProxy(upURL)
	defer proxy.Close()

	// A1, A2, A3 (user A, model llama) all hold the slot (re-enter).
	cancelA1, _ := startCancelable(t, proxy.URL, "/v1/chat/completions", "llama", "keyA")
	cancelA2, _ := startCancelable(t, proxy.URL, "/v1/chat/completions", "llama", "keyA")
	_, doneA3 := startCancelable(t, proxy.URL, "/v1/chat/completions", "llama", "keyA")

	// Let all three acquire the slot.
	time.Sleep(150 * time.Millisecond)

	// Cancel A1 and A2. They release via the reference count, but A3 is still in
	// flight, so the slot must stay held.
	cancelA1()
	cancelA2()

	// B (user B, model mistral) must be queued while A3 is in flight.
	bs, be, err := doRequest(t, proxy.URL, "/v1/chat/completions", "mistral", "keyB")
	if err != nil {
		t.Fatalf("B failed: %v", err)
	}
	_ = bs

	// A3 finished (with the fix, B only finishes after A3 + grace).
	a3e := <-doneA3

	// B must not finish before A3 + grace.
	if be.Before(a3e.Add(grace - 50*time.Millisecond)) {
		t.Errorf("B released before A3 finished + grace: be=%v a3e=%v", be, a3e)
	}
}

// lookup and tools are stubbed locally (never proxied) and must not touch the
// queue, even while it is held by another key/model.
func TestStubbedLookupAndTools(t *testing.T) {
	const sleep = 400 * time.Millisecond

	upstream := newFakeUpstream(t, sleep)
	defer upstream.Close()
	upURL, _ := url.Parse(upstream.URL)

	oldGrace := gracePeriod
	gracePeriod = sleep
	defer func() { gracePeriod = oldGrace }()

	proxy := newTestProxy(upURL)
	defer proxy.Close()

	// Hold the queue with a blocking request from keyA/llama.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		doRequest(t, proxy.URL, "/v1/chat/completions", "llama", "keyA")
	}()
	time.Sleep(100 * time.Millisecond) // let the holder acquire

	// lookup stub: immediate 200 [] (would be queued if it hit the proxy).
	req, err := http.NewRequest("POST", proxy.URL+"/upstream/mistral/v1/streams/lookup", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("building lookup request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer keyB")
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("lookup request failed: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("lookup status = %d, want 200", resp.StatusCode)
	}
	if string(body) != "[]" {
		t.Errorf("lookup body = %q, want []", string(body))
	}
	if elapsed := time.Since(start); elapsed > 150*time.Millisecond {
		t.Errorf("lookup was not local/immediate: %v", elapsed)
	}

	// tools stub: immediate 403 feature_disabled, for GET and POST alike
	// (the web UI polls it with POST, which a GET-only stub let through).
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		var reader io.Reader
		if method == http.MethodPost {
			reader = strings.NewReader("{}")
		}
		req, err := http.NewRequest(method, proxy.URL+"/upstream/mistral/tools", reader)
		if err != nil {
			t.Fatalf("building tools request: %v", err)
		}
		req.Header.Set("Authorization", "Bearer keyB")
		start := time.Now()
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("tools request failed: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("tools (%s) status = %d, want 403", method, resp.StatusCode)
		}
		if !strings.Contains(string(body), "feature_disabled") {
			t.Errorf("tools (%s) body = %q, want feature_disabled error", method, string(body))
		}
		if elapsed := time.Since(start); elapsed > 150*time.Millisecond {
			t.Errorf("tools (%s) was not local/immediate: %v", method, elapsed)
		}
	}

	wg.Wait()
}

// extractModel from /upstream/:model/... path
func TestExtractModelFromPath(t *testing.T) {
	r := &http.Request{URL: &url.URL{Path: "/upstream/llama-7/v1/chat/completions"}}
	if m, body := extractModel(r); m != "llama-7" || body != nil {
		t.Errorf("expected (llama-7, nil), got (%q, %v)", m, body)
	}
	r = &http.Request{URL: &url.URL{Path: "/upstream/my-model"}}
	if m, _ := extractModel(r); m != "my-model" {
		t.Errorf("expected my-model, got %q", m)
	}
}

// extractModel from the JSON body
func TestExtractModelFromBody(t *testing.T) {
	r := &http.Request{
		URL:  &url.URL{Path: "/v1/chat/completions"},
		Body: io.NopCloser(strings.NewReader(`{"model":"llama-7","prompt":"x"}`)),
	}
	if m, body := extractModel(r); m != "llama-7" || string(body) != `{"model":"llama-7","prompt":"x"}` {
		t.Errorf("expected (llama-7, original body), got (%q, %q)", m, body)
	}
	r = &http.Request{
		URL:  &url.URL{Path: "/v1/chat/completions"},
		Body: io.NopCloser(strings.NewReader(`{"prompt":"x"}`)),
	}
	if m, _ := extractModel(r); m != "" {
		t.Errorf("expected no model, got %q", m)
	}
}

func TestParseModelRewrites(t *testing.T) {
	t.Run("pairs", func(t *testing.T) {
		m := parseModelRewrites("client-a=upstream-a, client-b=upstream-b")
		if m["client-a"] != "upstream-a" || m["client-b"] != "upstream-b" || len(m) != 2 {
			t.Errorf("got %v, want both pairs", m)
		}
	})
	t.Run("malformed skipped", func(t *testing.T) {
		m := parseModelRewrites("no-equals, =x, y=, ok=good")
		if m["ok"] != "good" || len(m) != 1 {
			t.Errorf("got %v, want only ok=good", m)
		}
	})
	t.Run("empty", func(t *testing.T) {
		if m := parseModelRewrites(""); len(m) != 0 {
			t.Errorf("got %v, want empty", m)
		}
	})
}

func TestRewriteModelInBody(t *testing.T) {
	t.Run("match", func(t *testing.T) {
		out, changed := rewriteModelInBody([]byte(`{"model":"a","prompt":"x"}`), "a", "b")
		if !changed {
			t.Fatalf("expected a change")
		}
		var m struct {
			Model  string `json:"model"`
			Prompt string `json:"prompt"`
		}
		if err := json.Unmarshal(out, &m); err != nil || m.Model != "b" || m.Prompt != "x" {
			t.Errorf("got %q (%v)", out, err)
		}
	})
	t.Run("no model field", func(t *testing.T) {
		if _, changed := rewriteModelInBody([]byte(`{"prompt":"x"}`), "a", "b"); changed {
			t.Errorf("body without a model field was rewritten")
		}
	})
	t.Run("model mismatch", func(t *testing.T) {
		if _, changed := rewriteModelInBody([]byte(`{"model":"c"}`), "a", "b"); changed {
			t.Errorf("a non-matching model was rewritten")
		}
	})
	t.Run("not json", func(t *testing.T) {
		if _, changed := rewriteModelInBody([]byte(`not json`), "a", "b"); changed {
			t.Errorf("a non-JSON body was rewritten")
		}
	})
}

// vipSet builds the vipKeys map from raw API key strings (SHA-256 hashed),
// mirroring how LLM_PROXY_VIP_KEYS is configured in production.
func vipSet(keys ...string) map[[32]byte]bool {
	m := make(map[[32]byte]bool)
	for _, k := range keys {
		m[sha256.Sum256([]byte(k))] = true
	}
	return m
}

// A VIP key's response holds the model for the longer vipGracePeriod, so a
// different-model non-VIP key must wait until that period expires rather than
// the shorter gracePeriod.
func TestVipGracePeriodBlocksDifferentModel(t *testing.T) {
	const sleep = 300 * time.Millisecond
	const vipGrace = 900 * time.Millisecond

	upstream := newFakeUpstream(t, sleep)
	defer upstream.Close()
	upURL, _ := url.Parse(upstream.URL)

	oldGrace := gracePeriod
	oldVip := vipGracePeriod
	oldKeys := vipKeys
	gracePeriod = 200 * time.Millisecond
	vipGracePeriod = vipGrace
	vipKeys = vipSet("vipkey")
	defer func() { gracePeriod, vipGracePeriod, vipKeys = oldGrace, oldVip, oldKeys }()

	proxy := newTestProxy(upURL)
	defer proxy.Close()

	var (
		wg     sync.WaitGroup
		vs, ve time.Time
		bs, be time.Time
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(100 * time.Millisecond)
		bs, be, _ = doRequest(t, proxy.URL, "/v1/chat/completions", "mistral", "normalkey")
	}()
	vs, ve, err := doRequest(t, proxy.URL, "/v1/chat/completions", "llama", "vipkey")
	if err != nil {
		t.Fatalf("VIP request failed: %v", err)
	}
	wg.Wait()

	_ = vs
	// B (different model, non-VIP) must not be released before the VIP's longer
	// grace period has fully elapsed after the VIP's response.
	if be.Before(ve.Add(vipGrace - 100*time.Millisecond)) {
		t.Errorf("B released too early: B finish=%v VIP finish=%v vipGrace=%v", be, ve, vipGrace)
	}
	if be.Sub(bs) > vipGrace+2*sleep+2*time.Second {
		t.Errorf("B took too long: %v", be.Sub(bs))
	}
}

// A non-VIP key requesting the SAME model a VIP holds must not re-enter
// (intercept) the slot while the VIP's hold is active; it stays queued until the
// VIP's grace period expires. A normal holder would let a same-model key re-enter.
func TestVipBlocksSameModelInterception(t *testing.T) {
	const sleep = 300 * time.Millisecond
	const vipGrace = 900 * time.Millisecond

	upstream := newFakeUpstream(t, sleep)
	defer upstream.Close()
	upURL, _ := url.Parse(upstream.URL)

	oldGrace := gracePeriod
	oldVip := vipGracePeriod
	oldKeys := vipKeys
	gracePeriod = 200 * time.Millisecond
	vipGracePeriod = vipGrace
	vipKeys = vipSet("vipkey")
	defer func() { gracePeriod, vipGracePeriod, vipKeys = oldGrace, oldVip, oldKeys }()

	proxy := newTestProxy(upURL)
	defer proxy.Close()

	var (
		wg       sync.WaitGroup
		nvs, nve time.Time
		vs, ve   time.Time
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(100 * time.Millisecond)
		nvs, nve, _ = doRequest(t, proxy.URL, "/v1/chat/completions", "llama", "normalkey") // SAME model as the VIP
	}()
	vs, ve, err := doRequest(t, proxy.URL, "/v1/chat/completions", "llama", "vipkey")
	if err != nil {
		t.Fatalf("VIP request failed: %v", err)
	}
	wg.Wait()

	_ = vs
	_ = ve
	// Without the VIP block the same-model non-VIP would re-enter immediately and
	// finish in ~sleep. With the block it must wait out the VIP's grace period, so
	// its total time clearly exceeds the (much shorter) sleep.
	if nve.Sub(nvs) < vipGrace {
		t.Errorf("non-VIP re-entered the VIP-held model too early: %v (expected >= vipGrace %v)", nve.Sub(nvs), vipGrace)
	}
}

// A VIP key can still re-enter the slot it holds (same model, same key) like a
// normal re-entry; the VIP block only applies to NON-VIP keys.
func TestVipSameKeyReenters(t *testing.T) {
	const sleep = 300 * time.Millisecond
	const vipGrace = 300 * time.Millisecond

	upstream := newFakeUpstream(t, sleep)
	defer upstream.Close()
	upURL, _ := url.Parse(upstream.URL)

	oldGrace := gracePeriod
	oldVip := vipGracePeriod
	oldKeys := vipKeys
	gracePeriod = 200 * time.Millisecond
	vipGracePeriod = vipGrace
	vipKeys = vipSet("vipkey")
	defer func() { gracePeriod, vipGracePeriod, vipKeys = oldGrace, oldVip, oldKeys }()

	proxy := newTestProxy(upURL)
	defer proxy.Close()

	var (
		wg       sync.WaitGroup
		v1s, v1e time.Time
		v2s, v2e time.Time
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(100 * time.Millisecond)
		v2s, v2e, _ = doRequest(t, proxy.URL, "/v1/chat/completions", "llama", "vipkey")
	}()
	v1s, v1e, err := doRequest(t, proxy.URL, "/v1/chat/completions", "llama", "vipkey")
	if err != nil {
		t.Fatalf("first VIP request failed: %v", err)
	}
	wg.Wait()

	// V2 (same VIP key, same model) must re-enter immediately, not queue.
	if v2e.Sub(v2s) > 2*sleep {
		t.Errorf("VIP re-entry was blocked, took %v (expected < %v)", v2e.Sub(v2s), 2*sleep)
	}
	_ = v1s
	_ = v1e
}

// A CORS preflight is a browser handshake, not a model request, so it must be
// answered immediately. If it entered the queue it could only proceed when the
// service reaches NONE — which never happens while other requests keep the slot
// — and the browser's real request behind the preflight would hang with it.
func TestOptionsPreflightBypassesQueue(t *testing.T) {
	const sleep = 400 * time.Millisecond

	upstream := newFakeUpstream(t, sleep)
	defer upstream.Close()
	upURL, _ := url.Parse(upstream.URL)

	proxy := newTestProxy(upURL)
	defer proxy.Close()

	// Hold the slot with a request that stays in flight.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		doRequest(t, proxy.URL, "/v1/chat/completions", "llama", "keyA")
	}()
	time.Sleep(100 * time.Millisecond) // let the holder acquire the slot

	for _, path := range []string{
		"/v1/chat/completions",
		"/upstream/llama/v1/chat/completions",
		"/v1/models",
	} {
		req, err := http.NewRequest(http.MethodOptions, proxy.URL+path, nil)
		if err != nil {
			t.Fatalf("building preflight: %v", err)
		}
		req.Header.Set("Origin", "https://example.org")
		req.Header.Set("Access-Control-Request-Method", "POST")
		req.Header.Set("Access-Control-Request-Headers", "authorization,content-type")

		start := time.Now()
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("preflight %s failed: %v", path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		elapsed := time.Since(start)

		if resp.StatusCode != http.StatusNoContent {
			t.Errorf("preflight %s status = %d, want 204", path, resp.StatusCode)
		}
		if len(body) != 0 {
			t.Errorf("preflight %s body = %q, want empty", path, body)
		}
		if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "*" {
			t.Errorf("preflight %s ACAO = %q, want *", path, got)
		}
		if got := resp.Header.Get("Access-Control-Allow-Headers"); got != "authorization,content-type" {
			t.Errorf("preflight %s Allow-Headers = %q, want the requested headers", path, got)
		}
		if got := resp.Header.Get("Access-Control-Allow-Methods"); !strings.Contains(got, "POST") {
			t.Errorf("preflight %s Allow-Methods = %q, want it to include POST", path, got)
		}
		if elapsed > 150*time.Millisecond {
			t.Errorf("preflight %s was queued behind the holder: %v", path, elapsed)
		}
	}
	wg.Wait()
}

// A non-POST request takes a slot holder exactly like a POST does, so abandoning
// one (an SSE-resume GET, a closed browser tab) must free that holder when the
// proxy fails the request. A holder left behind keeps the slot occupied and the
// next request is only let through by the hard cap.
func TestCanceledGetReleasesSlot(t *testing.T) {
	const grace = 150 * time.Millisecond
	const holdCap = 30 * time.Second // the hard cap must not be what frees the slot

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			<-r.Context().Done() // a stream the client abandons
			return
		}
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer upstream.Close()
	upURL, _ := url.Parse(upstream.URL)

	oldGrace, oldCap := gracePeriod, hardCap
	gracePeriod, hardCap = grace, holdCap
	defer func() { gracePeriod, hardCap = oldGrace, oldCap }()

	proxy := newTestProxy(upURL)
	defer proxy.Close()

	// 1. make the proxy hold a named model, so a request for another model later
	//    has to wait for the slot to be freed rather than re-enter it.
	if _, _, err := doRequest(t, proxy.URL, "/v1/chat/completions", "llama", "keyA"); err != nil {
		t.Fatalf("POST holding the model: %v", err)
	}
	time.Sleep(grace + 100*time.Millisecond)

	// 2. a GET on an LLM path that the client abandons.
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, proxy.URL+"/upstream/llama/v1/chat/completions", nil)
	if err != nil {
		t.Fatalf("building GET: %v", err)
	}
	req.Header.Set("Authorization", "keyA")
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_, _ = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
		}
	}()
	time.Sleep(150 * time.Millisecond) // let it acquire the slot and reach the upstream
	cancel()
	time.Sleep(200 * time.Millisecond) // let the proxy error handler run

	// 3. another model must be let through after the grace period, not after the
	//    hold cap.
	start := time.Now()
	if _, _, err := doRequest(t, proxy.URL, "/v1/chat/completions", "mistral", "keyB"); err != nil {
		t.Fatalf("POST after the canceled GET: %v", err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("the canceled GET did not free its holder: the next request waited %v (grace %v, hold cap %v)", took, grace, holdCap)
	}
}

// newEchoUpstream returns an upstream that echoes the model named in the
// request body into its own response model field (plain JSON, or SSE when the
// body has "stream": true), and a getter for the model the upstream last saw.
func newEchoUpstream(t *testing.T, sleep time.Duration) (*httptest.Server, func() string) {
	t.Helper()
	var (
		mu   sync.Mutex
		last string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		_ = json.Unmarshal(body, &req)
		mu.Lock()
		last = req.Model
		mu.Unlock()
		time.Sleep(sleep)
		if req.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			f := w.(http.Flusher)
			fmt.Fprintf(w, "data: {\"model\":%q,\"id\":\"1\"}\n\n", req.Model)
			f.Flush()
			fmt.Fprint(w, "data: [DONE]\n\n")
			f.Flush()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"model":%q,"id":"1"}`, req.Model)
	}))
	return srv, func() string {
		mu.Lock()
		defer mu.Unlock()
		return last
	}
}

// A request that names a mapped model must reach the upstream under the
// upstream name, and the upstream's answer must come back under the client
// name: neither direction may leak the other name.
func TestRequestAndResponseModelRewritten(t *testing.T) {
	old := modelRewrites
	modelRewrites = map[string]string{"client-model": "upstream-model"}
	defer func() { modelRewrites = old }()

	upstream, received := newEchoUpstream(t, 50*time.Millisecond)
	defer upstream.Close()
	upURL, _ := url.Parse(upstream.URL)
	proxy := newTestProxy(upURL)
	defer proxy.Close()

	req, _ := http.NewRequest("POST", proxy.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"client-model","prompt":"x"}`))
	req.Header.Set("Authorization", "keyA")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%q)", resp.StatusCode, data)
	}
	if got := received(); got != "upstream-model" {
		t.Errorf("upstream saw model %q, want upstream-model", got)
	}
	var out struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("response is not JSON: %v (%q)", err, data)
	}
	if out.Model != "client-model" {
		t.Errorf("client saw model %q, want client-model", out.Model)
	}
	if resp.ContentLength < 0 || int(resp.ContentLength) != len(data) {
		t.Errorf("ContentLength %d does not match body length %d", resp.ContentLength, len(data))
	}
}

// The same contract for streaming (SSE) responses: every data line must carry
// the client name, the stream must stay a stream, and the terminator survives.
func TestSSEModelRewritten(t *testing.T) {
	old := modelRewrites
	modelRewrites = map[string]string{"client-model": "upstream-model"}
	defer func() { modelRewrites = old }()

	upstream, received := newEchoUpstream(t, 50*time.Millisecond)
	defer upstream.Close()
	upURL, _ := url.Parse(upstream.URL)
	proxy := newTestProxy(upURL)
	defer proxy.Close()

	req, _ := http.NewRequest("POST", proxy.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"client-model","stream":true}`))
	req.Header.Set("Authorization", "keyA")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	s := string(data)
	if strings.Contains(s, "upstream-model") {
		t.Errorf("upstream name leaked into the SSE stream: %s", s)
	}
	if !strings.Contains(s, `"model":"client-model"`) {
		t.Errorf("client name missing from SSE chunks: %s", s)
	}
	if !strings.Contains(s, "data: [DONE]") {
		t.Errorf("SSE terminator lost: %s", s)
	}
	if got := received(); got != "upstream-model" {
		t.Errorf("upstream saw model %q, want upstream-model", got)
	}
}

// A model that is not in the rewrite map must pass through unchanged in both
// directions, and a request that names no model must be rewritten neither
// way.
func TestUnmappedModelPassesThrough(t *testing.T) {
	old := modelRewrites
	modelRewrites = map[string]string{"other-model": "other-upstream"}
	defer func() { modelRewrites = old }()

	upstream, received := newEchoUpstream(t, 50*time.Millisecond)
	defer upstream.Close()
	upURL, _ := url.Parse(upstream.URL)
	proxy := newTestProxy(upURL)
	defer proxy.Close()

	for _, body := range []string{`{"model":"my-model"}`, `{"prompt":"x"}`} {
		req, _ := http.NewRequest("POST", proxy.URL+"/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "keyA")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request %q failed: %v", body, err)
		}
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		want := "my-model"
		if body == `{"prompt":"x"}` {
			want = ""
		}
		if got := received(); got != want {
			t.Errorf("request %q: upstream saw model %q, want %q", body, got, want)
		}
		var out struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(data, &out)
		if out.Model != want {
			t.Errorf("request %q: client saw model %q, want %q", body, out.Model, want)
		}
	}
}

// Two aliases of one real model must share its slot like same-model requests,
// while a different real model still queues behind them.
func TestRewrittenAliasesShareSlot(t *testing.T) {
	const sleep = 400 * time.Millisecond
	const grace = 300 * time.Millisecond

	upstream := newFakeUpstream(t, sleep)
	defer upstream.Close()
	upURL, _ := url.Parse(upstream.URL)

	oldGrace := gracePeriod
	oldRewrites := modelRewrites
	gracePeriod = grace
	modelRewrites = map[string]string{
		"alias-a": "real-model",
		"alias-b": "real-model",
	}
	defer func() { gracePeriod, modelRewrites = oldGrace, oldRewrites }()

	proxy := newTestProxy(upURL)
	defer proxy.Close()

	var (
		wg       sync.WaitGroup
		a1s, a1e time.Time
		bs, be   time.Time
		cs, ce   time.Time
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		time.Sleep(100 * time.Millisecond)
		bs, be, _ = doRequest(t, proxy.URL, "/v1/chat/completions", "alias-b", "keyB")
	}()
	go func() {
		defer wg.Done()
		time.Sleep(100 * time.Millisecond)
		cs, ce, _ = doRequest(t, proxy.URL, "/v1/chat/completions", "other-model", "keyC")
	}()
	a1s, a1e, err := doRequest(t, proxy.URL, "/v1/chat/completions", "alias-a", "keyA")
	if err != nil {
		t.Fatalf("A1 failed: %v", err)
	}
	wg.Wait()

	if be.Sub(bs) > 2*sleep {
		t.Errorf("B (an alias of the held real model) should re-enter, took %v (expected < %v)", be.Sub(bs), 2*sleep)
	}
	if a1e.Sub(a1s) > 2*sleep {
		t.Errorf("A1 was unexpectedly blocked, took %v", a1e.Sub(a1s) )
	}
	lastA := a1e
	if be.After(lastA) {
		lastA = be
	}
	// C (a different real model) must not be released before the later of
	// A1/B finished + grace period.
	earliestC := lastA.Add(grace)
	if ce.Before(earliestC.Add(-100 * time.Millisecond)) {
		t.Errorf("C released too early: finished %v, but grace ends at %v", ce, earliestC)
	}
	if ce.Sub(cs) > 3*time.Second {
		t.Errorf("C took too long: %v (grace %v)", ce.Sub(cs), grace)
	}
}

// VIP matching hashes the key without the Authorization scheme: a digest
// configured over the bare key matches a client sending "Bearer <key>", and a
// digest configured over the header as sent (the legacy form) still matches.
func TestVIPKeyMatchesBearerScheme(t *testing.T) {
	oldKeys := vipKeys
	defer func() { vipKeys = oldKeys }()

	vipKeys = vipSet("sk-vip")
	if !isVIPKey("sk-vip") {
		t.Error("bare key must match its own digest")
	}
	if !isVIPKey("Bearer sk-vip") {
		t.Error("the Bearer-prefixed header must match the bare key digest")
	}
	if !isVIPKey("bearer sk-vip") {
		t.Error("the scheme is case-insensitive and must still match")
	}
	if isVIPKey("Bearer sk-other") {
		t.Error("an unconfigured key must not match")
	}
	if isVIPKey("") {
		t.Error("an empty header must not match")
	}

	vipKeys = vipSet("Bearer sk-legacy")
	if !isVIPKey("Bearer sk-legacy") {
		t.Error("a digest configured over the header as sent must still match")
	}
}
