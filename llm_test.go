package main

import (
	"crypto/sha256"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
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
	sq.Hold()
	sq.Release(time.Minute, 0)
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
	sq.Hold()
	sq.Release(time.Minute, 30*time.Second)
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

type colorStub struct{}

func (c *colorStub) SetTaskColor(string) {}
