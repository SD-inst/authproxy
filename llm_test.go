package main

import (
	"crypto/sha256"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/rkfg/authproxy/proxy"
	"github.com/rkfg/authproxy/servicequeue"
)

// newTestQueue builds a service queue whose status-page updates are drained, the
// way main.go's progress consumer would.
func newTestQueue() *servicequeue.ServiceQueue {
	sq, _ := newTestQueueWithUpdates()
	return sq
}

// newTestQueueWithUpdates returns the queue and its update channel instead of
// draining it, for tests that assert which states a request leaves behind. The
// channel is buffered, so ignoring it cannot wedge the queue.
func newTestQueueWithUpdates() (*servicequeue.ServiceQueue, chan servicequeue.SvcUpdate) {
	svcChan := make(chan servicequeue.SvcUpdate, 100)
	sq := servicequeue.NewServiceQueue(svcChan)
	return sq, svcChan
}

// llama-swap keeps whatever model it loaded, and its container is never stopped
// for idleness, so after an authproxy restart a loaded LLM survives with nothing
// unloaded. The startup cleanup must therefore belong to the LLM: the first
// request for another GPU service has to drop the model, or both sit in VRAM at
// once and the switch OOMs.
func TestStartupCleanupUnloadsLLMOnFirstHandover(t *testing.T) {
	var unloads atomic.Int32
	paths := make(chan string, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case paths <- r.URL.Path:
		default:
		}
		if r.URL.Path == "/unload" {
			unloads.Add(1)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	// LLM_URL carries no path (service_url.go) and llama-swap serves its control
	// endpoints at the root, so the unload must land on /unload.
	target, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse target: %s", err)
	}
	llm := &llmbalancer{target: target, client: *srv.Client()}
	sq := newTestQueue()
	sq.CF = llm.startupCleanup()
	if sq.CF.Service != servicequeue.LLM {
		t.Fatalf("the pending startup cleanup must belong to the LLM, got %v", sq.CF.Service)
	}

	sq.Lock()
	changed := sq.AwaitReent(servicequeue.SD) // the first A1111 join after startup
	sq.Unlock()

	if !changed {
		t.Fatal("the queue must report a service change")
	}
	if got := unloads.Load(); got != 1 {
		t.Fatalf("the LLM model must be unloaded once before another service takes the slot, got %d calls", got)
	}
	if p := <-paths; p != "/unload" {
		t.Fatalf("the unload request must go to /unload, got %q", p)
	}
	if sq.CF != nil {
		t.Fatalf("the startup cleanup must be consumed, left %v", sq.CF.Service)
	}
}

// The cleanup is not run when the LLM takes the slot again: unloading llama-swap
// to hand the GPU to llama-swap would drop the model the request needs.
func TestLLMHandoverDoesNotUnloadItself(t *testing.T) {
	var unloads atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		unloads.Add(1)
	}))
	defer srv.Close()

	target, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse target: %s", err)
	}
	llm := &llmbalancer{target: target, client: *srv.Client()}
	sq := newTestQueue()
	sq.CF = llm.startupCleanup()

	sq.Lock()
	sq.AwaitReent(servicequeue.LLM)
	sq.Unlock()

	if got := unloads.Load(); got != 0 {
		t.Fatalf("handing the slot to the LLM must not unload it, got %d calls", got)
	}
}

// A1111 loads its checkpoint when a client joins, so nothing is resident on its
// behalf at startup and registering its handlers must not claim the pending
// cleanup.
func TestSDQueueHandlersArmNoCleanupAtStartup(t *testing.T) {
	sq := newTestQueue()
	addSDQueueHandlers(echo.New(), sq, &colorStub{})
	if sq.CF != nil {
		t.Fatalf("no cleanup may be pending at startup, got one for %v", sq.CF.Service)
	}
}

// The WAIT hold between requests is a VIP-only privilege: a VIP key keeps the
// slot for the configured grace period so its next request finds the model
// loaded, and every other key frees it at once so the next queued task — another
// model or another GPU service — runs.
func TestLLMGraceDelayHoldsOnlyForVIPKeys(t *testing.T) {
	oldKeys, oldGrace := vipKeys, vipGracePeriod
	defer func() { vipKeys, vipGracePeriod = oldKeys, oldGrace }()
	vipKeys = map[[32]byte]bool{sha256.Sum256([]byte("sk-vip")): true}
	vipGracePeriod = 30 * time.Second

	request := func(method, path, key string) *http.Request {
		var body io.Reader
		if method == http.MethodPost {
			body = strings.NewReader(`{"model":"llama"}`)
		}
		r, err := http.NewRequest(method, "http://authproxy"+path, body)
		if err != nil {
			t.Fatalf("building request: %s", err)
		}
		r.Header.Set("Authorization", key)
		return r
	}

	for _, path := range []string{"/v1/chat/completions", "/v1/completions", "/v1/embeddings"} {
		if d := llmGraceDelay(request(http.MethodPost, path, "Bearer sk-vip")); d != 30*time.Second {
			t.Errorf("VIP POST %s: expected the 30s hold, got %s", path, d)
		}
	}
	// The bare key hashes to the configured digest just the same.
	if d := llmGraceDelay(request(http.MethodPost, "/v1/chat/completions", "sk-vip")); d != 30*time.Second {
		t.Errorf("VIP with a bare key: expected the 30s hold, got %s", d)
	}
	// Any other key frees the slot as soon as its body closes.
	if d := llmGraceDelay(request(http.MethodPost, "/v1/chat/completions", "sk-plain")); d != 0 {
		t.Errorf("non-VIP key: expected no hold, got %s", d)
	}
	// A VIP holds only on generation calls: not on another path, not on a GET.
	if d := llmGraceDelay(request(http.MethodPost, "/v1/models", "sk-vip")); d != 0 {
		t.Errorf("VIP on a non-generation path: expected no hold, got %s", d)
	}
	if d := llmGraceDelay(request(http.MethodGet, "/v1/chat/completions", "sk-vip")); d != 0 {
		t.Errorf("VIP GET: expected no hold, got %s", d)
	}
	// With no VIP keys configured nobody holds anything.
	vipKeys = nil
	if d := llmGraceDelay(request(http.MethodPost, "/v1/chat/completions", "sk-vip")); d != 0 {
		t.Errorf("no VIP keys configured: expected no hold, got %s", d)
	}
}

// A release with no grace — what every non-VIP request gets — takes the queue
// straight back to NONE. WAIT is only ever published for a VIP hold.
func TestReleaseWithoutGraceSkipsWait(t *testing.T) {
	sq, updates := newTestQueueWithUpdates()
	sq.Lock()
	sq.SetService(servicequeue.LLM)
	hold := sq.Hold()
	sq.Release(hold, time.Minute, 0)
	sq.Unlock()

	var last servicequeue.SvcUpdate
	for {
		select {
		case u := <-updates:
			if u.Type == servicequeue.WAIT {
				t.Fatalf("a release with no grace published WAIT/%v", u.WaitType)
			}
			last = u
		default:
			if last.Type != servicequeue.NONE {
				t.Fatalf("the slot must be back to NONE, the queue last said %v", last.Type)
			}
			return
		}
	}
}

// A release with a grace period — the VIP path — keeps the slot: the queue goes
// to WAIT and still names the LLM as the service holding it, so the next request
// for anything else stays queued until the hold expires.
func TestReleaseWithGraceHoldsTheSlot(t *testing.T) {
	sq, updates := newTestQueueWithUpdates()
	sq.Lock()
	sq.SetService(servicequeue.LLM)
	hold := sq.Hold()
	sq.Release(hold, time.Minute, 30*time.Second)
	sq.Unlock()

	for {
		select {
		case u := <-updates:
			if u.Type == servicequeue.WAIT {
				if u.WaitType != servicequeue.LLM {
					t.Fatalf("the hold must name the LLM as the holder, got WAIT/%v", u.WaitType)
				}
				return
			}
		case <-time.After(time.Second):
			t.Fatal("the queue never entered WAIT after a held release")
		}
	}
}

// The hold bound is the counterpart of the VIP grace: a VIP may keep the slot
// for as long as it likes (its hold is what keeps the model loaded), while any
// other key is bounded by llm_nonvip_max_hold from the moment it takes the slot,
// so several parallel threads of one key cannot occupy the queue indefinitely.
func TestLLMHoldLimitBoundsOnlyNonVIPKeys(t *testing.T) {
	oldKeys, oldLimit := vipKeys, nonVIPMaxHold
	defer func() { vipKeys, nonVIPMaxHold = oldKeys, oldLimit }()
	vipKeys = map[[32]byte]bool{sha256.Sum256([]byte("sk-vip")): true}
	nonVIPMaxHold = time.Minute

	if d := llmHoldLimit("Bearer sk-vip"); d != 0 {
		t.Errorf("VIP key: expected no bound, got %s", d)
	}
	for _, key := range []string{"sk-plain", "Bearer sk-plain", ""} {
		if d := llmHoldLimit(key); d != time.Minute {
			t.Errorf("key %q: expected the configured %s bound, got %s", key, time.Minute, d)
		}
	}
	// llm_nonvip_max_hold: "0s" switches the bound off for everyone.
	nonVIPMaxHold = 0
	if d := llmHoldLimit("sk-plain"); d != 0 {
		t.Errorf("with the bound disabled: expected 0, got %s", d)
	}
}

// The bound seen from the client's side, through the real proxy chain: a non-VIP
// key with two parallel threads streaming from one model cannot keep the queue.
// The deadline was armed when the first thread took the slot, and the second one
// re-entered it without moving it, so the slot goes back to NONE while both
// requests are still streaming and a task queued behind them gets the GPU. A VIP
// key is exempt — nothing takes the slot away from it while it is working.
func TestLLMHoldDeadlineFreesTheQueueForNonVIP(t *testing.T) {
	const (
		deadline = 100 * time.Millisecond // llm_nonvip_max_hold
		stream   = 800 * time.Millisecond // how long the model keeps generating
	)
	oldLimit, oldKeys, oldGrace := nonVIPMaxHold, vipKeys, vipGracePeriod
	defer func() { nonVIPMaxHold, vipKeys, vipGracePeriod = oldLimit, oldKeys, oldGrace }()
	nonVIPMaxHold = deadline
	vipGracePeriod = 50 * time.Millisecond

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/unload" {
			return // the handover drops the model, as any handover does
		}
		time.Sleep(stream)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"model":"llama"}`)
	}))
	defer upstream.Close()
	target, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse target: %s", err)
	}
	sq := newTestQueue()
	llm := &llmbalancer{sq: sq, target: target, client: *upstream.Client()}

	e := echo.New()
	e.Use(proxy.NewProxyWrapper(target, llm.interceptor()))
	e.POST("/v1/chat/completions", func(c echo.Context) error {
		return c.String(http.StatusNotImplemented, "the proxy middleware must not chain to the handler")
	})
	front := httptest.NewServer(e)
	defer front.Close()

	post := func(t *testing.T, key string) {
		req, err := http.NewRequest(http.MethodPost, front.URL+"/v1/chat/completions", strings.NewReader(`{"model":"llama","messages":[{"role":"user","content":"hi"}]}`))
		if err != nil {
			t.Errorf("building request: %s", err)
			return
		}
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Errorf("proxied request: %s", err)
			return
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}

	// waitForFree returns once the slot is back to NONE, so one case cannot
	// start behind the leftovers of the previous one.
	waitForFree := func(t *testing.T) {
		t.Helper()
		timeout := time.After(5 * time.Second)
		for {
			sq.Lock()
			free := sq.Service() == servicequeue.NONE
			sq.Unlock()
			if free {
				return
			}
			select {
			case <-timeout:
				t.Fatal("the slot never went back to NONE")
			case <-time.After(10 * time.Millisecond):
			}
		}
	}

	for _, tc := range []struct {
		name string
		key  string
	}{
		{"non-VIP", "sk-plain"},
		{"VIP", "sk-vip"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vipKeys = nil
			if tc.name == "VIP" {
				vipKeys = map[[32]byte]bool{sha256.Sum256([]byte("sk-vip")): true}
			}
			waitForFree(t)

			start := time.Now()
			var wg sync.WaitGroup
			for i := 0; i < 2; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					post(t, tc.key)
				}()
			}
			// Both threads are in the slot well before the deadline; A1111 joins
			// the queue behind them and must wait for the slot, not for the model.
			time.Sleep(deadline / 3)
			acquired := make(chan time.Time, 1)
			go func() {
				sq.Lock()
				sq.AwaitReent(servicequeue.SD)
				now := time.Now()
				sq.SetService(servicequeue.NONE) // A1111 is done with the GPU
				sq.Unlock()
				acquired <- now
			}()

			var got time.Time
			select {
			case got = <-acquired:
			case <-time.After(stream + 5*time.Second):
				t.Fatal("the queued A1111 request never got the slot")
			}
			elapsed := got.Sub(start)
			if tc.name == "non-VIP" {
				// The point of the bound: the slot changes hands while the key is
				// still generating, and not one moment earlier.
				if elapsed < deadline-5*time.Millisecond {
					t.Errorf("A1111 got the slot %v after the start, before the %v hold bound", elapsed, deadline)
				}
				if elapsed > stream/2 {
					t.Errorf("A1111 waited %v behind two non-VIP threads, want the %v bound to free the slot while they stream (%v)", elapsed, deadline, stream)
				}
			} else if elapsed < stream {
				t.Errorf("a VIP was cut short: A1111 got the slot %v after the start, want no earlier than the end of its requests (%v)", elapsed, stream)
			}
			wg.Wait()
		})
	}
}

type colorStub struct{}

func (c *colorStub) SetTaskColor(string) {}
