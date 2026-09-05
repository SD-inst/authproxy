package main

import (
	"context"
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
	rp := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = upstream.Scheme
			req.URL.Host = upstream.Host
		},
		ModifyResponse: func(resp *http.Response) error {
			return sq.ServiceCloserWithAfterBody(servicequeue.LLM, func(path string) bool {
				return isLLMPath(path)
			}, hardCap, true, graceDelay)(resp.Request, resp)
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			// Mirror the real proxy: a canceled/failed request releases its
			// holder via the reference count.
			if r.Method == "POST" && isLLMPath(r.URL.Path) {
				sq.Lock()
				sq.Release(hardCap, graceDelay(r))
				sq.Unlock()
			}
			w.WriteHeader(http.StatusBadGateway)
			fmt.Fprintf(w, "Proxy error: %s\n", err)
		},
	}
	return httptest.NewServer(newHandler(lp, rp))
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
	if m := extractModel(r); m != "llama-7" {
		t.Errorf("expected llama-7, got %q", m)
	}
	r = &http.Request{URL: &url.URL{Path: "/upstream/my-model"}}
	if m := extractModel(r); m != "my-model" {
		t.Errorf("expected my-model, got %q", m)
	}
}
