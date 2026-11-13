package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
)

// newCUIObjectInfoEcho builds an Echo instance with the /cui/api/object_info
// route wired the way main.go does it, using the given container manager and
// backend target.
func newCUIObjectInfoEcho(m *containerManager, target *url.URL) *echo.Echo {
	e := echo.New()
	real := composeMW(middleware.Rewrite(map[string]string{"/cui/api/object_info*": "/api/object_info$1"}), newObjectInfoProxy(target, m))
	e.Any("/cui/api/object_info", m.cuiObjectInfoHandler(real))
	return e
}

// setRunning drives the manager's lifecycle view directly, standing in for the
// idle-stop timer having stopped the container.
func setRunning(m *containerManager, svc string, running bool) {
	st := m.states[svc]
	st.mu.Lock()
	st.running = running
	if running {
		st.boot.Add(1)
	}
	st.mu.Unlock()
}

// nodeDefsServer answers /api/object_info with a body that changes per call, so
// a test can tell a proxied response from a replayed one. It records the path
// the backend actually received and how many times it was asked.
func nodeDefsServer(path *atomic.Value, hits *atomic.Int32) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path.Store(r.URL.Path)
		n := hits.Add(1)
		w.Header().Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		fmt.Fprintf(w, `{"KSampler":{"n":%d},"EmptyLatentImage":{"n":%d}}`, n, n)
	}))
}

func fetchObjectInfo(t *testing.T, srv *httptest.Server) (string, http.Header) {
	t.Helper()
	res, err := http.Get(srv.URL + "/cui/api/object_info")
	if err != nil {
		t.Fatalf("GET failed: %s", err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("body read failed: %s", err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", res.StatusCode, body)
	}
	return string(body), res.Header
}

// While the container is running the fetch must be proxied, and the backend must
// receive the rewritten /api/object_info path (not the raw /cui path).
func TestCUIObjectInfoProxiesWhenRunning(t *testing.T) {
	var path atomic.Value
	var hits atomic.Int32
	path.Store("")
	backend := nodeDefsServer(&path, &hits)
	defer backend.Close()
	target, _ := url.Parse(backend.URL)

	m := newTestManager(&fakeWd{enabled: true, resp: "ok"})
	srv := httptest.NewServer(newCUIObjectInfoEcho(m, target))
	defer srv.Close()

	setRunning(m, comfyService, true)
	if got, _ := fetchObjectInfo(t, srv); got != `{"KSampler":{"n":1},"EmptyLatentImage":{"n":1}}` {
		t.Fatalf("unexpected proxied body: %s", got)
	}
	if p := path.Load().(string); p != "/api/object_info" {
		t.Fatalf("backend received path %q; expected /api/object_info", p)
	}
}

// The case the idle tabs produce every hour: the container runs, the page loads
// and its definitions are fetched, the container goes idle and stops, the tab's
// websocket reconnects and asks again. The second ask must be answered from the
// captured body without touching the container.
func TestCUIObjectInfoReplaysCapturedBodyWhenStopped(t *testing.T) {
	var path atomic.Value
	var hits atomic.Int32
	path.Store("")
	backend := nodeDefsServer(&path, &hits)
	defer backend.Close()
	target, _ := url.Parse(backend.URL)

	m := newTestManager(&fakeWd{enabled: true, resp: "ok"})
	srv := httptest.NewServer(newCUIObjectInfoEcho(m, target))
	defer srv.Close()

	setRunning(m, comfyService, true)
	first, firstHeader := fetchObjectInfo(t, srv)

	setRunning(m, comfyService, false)
	second, secondHeader := fetchObjectInfo(t, srv)

	if second != first {
		t.Fatalf("replayed body differs from the captured one:\n got %s\nwant %s", second, first)
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("backend was asked %d times; the stopped container was proxied", n)
	}
	if m.isRunning(comfyService) {
		t.Fatal("the stopped container was started by the reconnect fetch")
	}
	if ct := m.bootGeneration(comfyService); ct != 1 {
		t.Fatalf("boot generation is %d; the replay must answer within the boot that captured it", ct)
	}
	if secondHeader.Get(echo.HeaderContentType) != firstHeader.Get(echo.HeaderContentType) {
		t.Fatalf("replayed content type %q differs from %q", secondHeader.Get(echo.HeaderContentType), firstHeader.Get(echo.HeaderContentType))
	}
}

// A fetch for a boot that has never produced a body has nothing to replay, so it
// is the one case allowed to start the container.
func TestCUIObjectInfoColdBootStartsContainer(t *testing.T) {
	var path atomic.Value
	var hits atomic.Int32
	path.Store("")
	backend := nodeDefsServer(&path, &hits)
	defer backend.Close()
	target, _ := url.Parse(backend.URL)

	wd := &fakeWd{enabled: true, resp: "ok"}
	m := newTestManager(wd)
	srv := httptest.NewServer(newCUIObjectInfoEcho(m, target))
	defer srv.Close()

	if got, _ := fetchObjectInfo(t, srv); got != `{"KSampler":{"n":1},"EmptyLatentImage":{"n":1}}` {
		t.Fatalf("unexpected body: %s", got)
	}
	var started int
	for _, cmd := range wd.commands {
		if cmd == "start "+comfyService {
			started++
		}
	}
	if started != 1 {
		t.Fatalf("watchdog got %d start commands (%v); the cold fetch had to start the container", started, wd.commands)
	}
	if !m.isRunning(comfyService) {
		t.Fatal("the cold fetch did not leave the container running")
	}
}

// Definitions captured before a restart belong to the boot that produced them.
// Custom nodes change on restarts, so the next fetch after one goes to the
// container instead of replaying what came before it.
func TestCUIObjectInfoSnapshotDoesNotOutliveItsBoot(t *testing.T) {
	var path atomic.Value
	var hits atomic.Int32
	path.Store("")
	backend := nodeDefsServer(&path, &hits)
	defer backend.Close()
	target, _ := url.Parse(backend.URL)

	wd := &fakeWd{enabled: true, resp: "ok"}
	m := newTestManager(wd)
	srv := httptest.NewServer(newCUIObjectInfoEcho(m, target))
	defer srv.Close()

	setRunning(m, comfyService, true)
	fetchObjectInfo(t, srv)

	if err := m.control(comfyService, opRestart); err != nil {
		t.Fatalf("restart failed: %s", err)
	}
	setRunning(m, comfyService, false)

	if got, _ := fetchObjectInfo(t, srv); got != `{"KSampler":{"n":2},"EmptyLatentImage":{"n":2}}` {
		t.Fatalf("body after a restart came from the previous boot: %s", got)
	}
	if n := hits.Load(); n != 2 {
		t.Fatalf("backend was asked %d times; expected a fresh fetch after the restart", n)
	}
}

// A body that never arrived whole must not be cached: the frontend reads a
// valid-but-truncated catalogue as the truth and rebuilds the open graph around
// the nodes it no longer has.
func TestCUIObjectInfoIgnoresTruncatedBody(t *testing.T) {
	var path atomic.Value
	var hits atomic.Int32
	path.Store("")
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path.Store(r.URL.Path)
		n := hits.Add(1)
		w.Header().Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		if n == 1 {
			// The upstream dying mid-response: the client sees EOF on a body
			// that is not a finished object.
			w.Write([]byte(`{"KSampler":{"n":1`))
			return
		}
		fmt.Fprintf(w, `{"KSampler":{"n":%d}}`, n)
	}))
	defer backend.Close()
	target, _ := url.Parse(backend.URL)

	m := newTestManager(&fakeWd{enabled: true, resp: "ok"})
	srv := httptest.NewServer(newCUIObjectInfoEcho(m, target))
	defer srv.Close()

	setRunning(m, comfyService, true)
	fetchObjectInfo(t, srv)

	setRunning(m, comfyService, false)
	if got, _ := fetchObjectInfo(t, srv); got != `{"KSampler":{"n":2}}` {
		t.Fatalf("a truncated body was replayed: %s", got)
	}
}

// The cache keeps one body per boot, replays it only for that boot, and lets the
// next attempt through when a capture was abandoned.
func TestObjectInfoCacheIsScopedToItsBoot(t *testing.T) {
	c := &objectInfoCache{}
	if !c.beginCapture(1) {
		t.Fatal("first capture was refused")
	}
	if c.beginCapture(1) {
		t.Fatal("a second capture was claimed while one was in flight")
	}
	c.finishCapture()
	if !c.beginCapture(1) {
		t.Fatal("an abandoned capture was not retryable")
	}
	c.store([]byte(`{"KSampler":{}}`), echo.MIMEApplicationJSON, 1)
	c.finishCapture()
	if c.beginCapture(1) {
		t.Fatal("this boot was already captured")
	}
	if snap := c.snapshot(1); snap == nil || string(snap.body) != `{"KSampler":{}}` {
		t.Fatalf("snapshot for the captured boot is wrong: %v", snap)
	}
	if snap := c.snapshot(2); snap != nil {
		t.Fatalf("a snapshot from boot 1 was offered for boot 2: %s", snap.body)
	}
}
