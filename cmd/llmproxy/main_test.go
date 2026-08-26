package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
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
	ps := newProxyState()
	rp := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = upstream.Scheme
			req.URL.Host = upstream.Host
		},
	}
	return httptest.NewServer(newHandler(ps, rp))
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
	go func() {
		defer wg.Done()
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
	go func() {
		defer wg.Done()
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

// The upstream hangs on a completions request (stuck generation). A queued
// request with a different key (e.g. the web UI) must still get the slot once
// the hold cap expires, instead of waiting forever.
func TestHoldCapReleasesStuckSlot(t *testing.T) {
	const cap = 400 * time.Millisecond

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/upstream/llama/v1/chat/completions" {
			time.Sleep(3 * time.Second) // stuck generation: slow to finish
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<html>ok</html>")
	}))
	defer upstream.Close()
	upURL, _ := url.Parse(upstream.URL)

	oldHold := holdTimeout
	holdTimeout = cap
	defer func() { holdTimeout = oldHold }()

	proxy := newTestProxy(upURL)
	defer proxy.Close()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		doRequest(t, proxy.URL, "/upstream/llama/v1/chat/completions", "", "keyA")
	}()
	time.Sleep(100 * time.Millisecond) // let the holder acquire

	// The web UI equivalent: different key, no model — queued while keyA holds.
	req, err := http.NewRequest("GET", proxy.URL+"/upstream/mistral/", nil)
	if err != nil {
		t.Fatalf("building GET request: %s", err)
	}
	req.Header.Set("Authorization", "keyB")
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET request failed: %s", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	elapsed := time.Since(start)

	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET status = %d, want 200", resp.StatusCode)
	}
	if string(body) != "<html>ok</html>" {
		t.Errorf("GET body = %q, want the upstream page", string(body))
	}
	if elapsed < cap/2 {
		t.Errorf("GET was not queued behind the stuck holder: %v", elapsed)
	}
	if elapsed > cap+2*time.Second {
		t.Errorf("GET was not released by the hold cap in bounded time: %v", elapsed)
	}
	wg.Wait()
}

// A request that outlives the hold cap must not disturb the new holder when it
// finally finishes (its requestDone must be a no-op).
func TestStaleRequestDoneIsNoOp(t *testing.T) {
	ps := newProxyState()

	e1 := ps.acquire("llama", "keyA")
	// simulate the hold cap freeing the slot while the first request is still in flight
	ps.mu.Lock()
	ps.releaseLocked()
	ps.mu.Unlock()

	e2 := ps.acquire("mistral", "keyB")
	if e1 == e2 {
		t.Fatalf("epoch was not advanced on release")
	}

	// the old request finishes after the cap: must be a no-op
	ps.requestDone(e1, 0)
	ps.mu.Lock()
	busy, inFlight := ps.busy, ps.inFlight
	ps.mu.Unlock()
	if !busy || inFlight != 1 {
		t.Fatalf("stale requestDone disturbed the new holder: busy=%v inFlight=%d", busy, inFlight)
	}

	// the current holder finishes and releases
	ps.requestDone(e2, 0)
	ps.mu.Lock()
	busy, inFlight = ps.busy, ps.inFlight
	ps.mu.Unlock()
	if busy || inFlight != 0 {
		t.Fatalf("holder was not released: busy=%v inFlight=%d", busy, inFlight)
	}
}

// extractModel from /upstream/:model/... path
func TestExtractModelFromPath(t *testing.T) {
	r := &http.Request{URL: &url.URL{Path: "/upstream/llama-7/v1/chat/completions"}}
	if m := extractModel(r); m != "llama-7" {
		t.Errorf("expected llama-7, got %q", m)
	}
	r = &http.Request{URL: &url.URL{Path: "/upstream/my-model"}}
	if m := extractModel(r); m != "my-model" {
		t.Errorf("expected my-model, got %q", m)
	}
}
