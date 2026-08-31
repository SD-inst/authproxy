package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/rkfg/authproxy/watchdog"
)

// newCUIJobsEcho builds an Echo instance with the /cui/api/jobs route wired the
// way main.go does it, using the given container manager.
func newCUIJobsEcho(m *containerManager) *echo.Echo {
	e := echo.New()
	// A bogus target: the stub branch never proxies, so the address is never
	// contacted while the container is stopped.
	target, _ := url.Parse("http://127.0.0.1:1")
	real := composeMW(middleware.Rewrite(map[string]string{"/cui/api/jobs*": "/api/jobs$1"}), newCUIProxy(target))
	e.Any("/cui/api/jobs", m.cuiJobsHandler(real))
	return e
}

// While the container is stopped, the jobs poll must return the empty stub
// (200, no jobs) and must NOT start the container.
func TestCUIJobsStubWhenStopped(t *testing.T) {
	m := newContainerManager(watchdog.NewWatchdog("127.0.0.1:1"), time.Minute)
	srv := httptest.NewServer(newCUIJobsEcho(m))
	defer srv.Close()

	res, err := http.Get(srv.URL + "/cui/api/jobs?status=in_progress,pending")
	if err != nil {
		t.Fatalf("GET failed: %s", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", res.StatusCode)
	}
	var got map[string]any
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatalf("bad JSON: %s", err)
	}
	if jobs, ok := got["jobs"].([]any); !ok || len(jobs) != 0 {
		t.Fatalf("expected an empty jobs array, got %v", got["jobs"])
	}
	if m.isRunning("comfyui") {
		t.Fatal("jobs poll started the container; expected it to stay stopped")
	}
}

// When the container is running, the jobs poll must be proxied to the real
// backend, which receives the rewritten /api/jobs path (not the raw /cui path).
func TestCUIJobsProxiesWhenRunning(t *testing.T) {
	var backendPath atomic.Value
	backendPath.Store("")
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backendPath.Store(r.URL.Path)
		w.Header().Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		w.Write([]byte(`{"jobs":["x"],"pagination":{"offset":0,"limit":1,"total":1,"has_more":true}}`))
	}))
	defer backend.Close()
	target, _ := url.Parse(backend.URL)

	m := newContainerManager(watchdog.NewWatchdog("127.0.0.1:1"), time.Minute)
	e := echo.New()
	real := composeMW(middleware.Rewrite(map[string]string{"/cui/api/jobs*": "/api/jobs$1"}), newCUIProxy(target))
	e.Any("/cui/api/jobs", m.cuiJobsHandler(real))

	// Mark the container as running so the poll is proxied for real.
	st := m.states["comfyui"]
	st.mu.Lock()
	st.running = true
	st.mu.Unlock()

	srv := httptest.NewServer(e)
	defer srv.Close()

	res, err := http.Get(srv.URL + "/cui/api/jobs?status=in_progress,pending")
	if err != nil {
		t.Fatalf("GET failed: %s", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", res.StatusCode)
	}
	if p := backendPath.Load().(string); p != "/api/jobs" {
		t.Fatalf("backend received path %q; expected /api/jobs (rewrite dropped the query string)", p)
	}
}
