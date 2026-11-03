package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/rkfg/authproxy/servicequeue"
)

// newTestQueue builds a service queue whose status-page updates are drained, the
// way main.go's progress consumer would.
func newTestQueue() *servicequeue.ServiceQueue {
	svcChan := make(chan servicequeue.SvcUpdate, 100)
	go func() {
		for range svcChan {
		}
	}()
	return servicequeue.NewServiceQueue(svcChan)
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

type colorStub struct{}

func (c *colorStub) SetTaskColor(string) {}
