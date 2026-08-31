package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/rkfg/authproxy/watchdog"
)

// newCUIWSEcho builds an Echo instance with the /cui/ws route wired exactly the
// way main.go does it, using the given container manager.
func newCUIWSEcho(m *containerManager) *echo.Echo {
	e := echo.New()
	// A bogus but enabled watchdog: auto start/stop is on, so isRunning's view
	// (stopped at startup) drives the silent branch. The silent WS never calls
	// Exec, so the bogus address is never contacted.
	target, _ := url.Parse("http://127.0.0.1:1")
	real := composeMW(middleware.Rewrite(map[string]string{"/cui/ws*": "/ws$1"}), newCUIProxy(target))
	e.Any("/cui/ws", m.cuiWSHandler(real))
	return e
}

func dialCUIWS(t *testing.T, srv *httptest.Server) *websocket.Conn {
	t.Helper()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/cui/ws?clientId=test"
	var dialer websocket.Dialer
	ws, _, err := dialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("WS connect failed: %s", err)
	}
	return ws
}

// A websocket reconnect while the container is stopped must be accepted without
// error, must send no data, and must NOT start the container.
func TestCUIWSSilentWhenStopped(t *testing.T) {
	m := newContainerManager(watchdog.NewWatchdog("127.0.0.1:1"), time.Minute)
	srv := httptest.NewServer(newCUIWSEcho(m))
	defer srv.Close()

	ws := dialCUIWS(t, srv)
	defer ws.Close()

	// The WS connection must not have started the container.
	if m.isRunning("comfyui") {
		t.Fatal("WS connection started the container; expected it to stay stopped")
	}

	// The silent WS sends nothing while the container is stopped, so a read with
	// a deadline should time out rather than deliver a message.
	ws.SetReadDeadline(time.Now().Add(3 * silentWSPollInterval))
	if _, _, err := ws.ReadMessage(); err == nil {
		t.Fatal("silent WS sent data; expected no messages while the container is stopped")
	}
}

// When the container is running, the WS must be proxied to the real backend.
func TestCUIWSProxiesWhenRunning(t *testing.T) {
	var backendPath atomic.Value
	backendPath.Store("")
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Capture the path the backend receives; the rewrite must turn the
		// client's /cui/ws?clientId=… into /ws (comfyui only serves /ws).
		backendPath.Store(r.URL.Path)
		upg := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		conn, err := upg.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("backend WS upgrade failed: %s", err)
			return
		}
		defer conn.Close()
		if err := conn.WriteMessage(websocket.TextMessage, []byte("hello")); err != nil {
			t.Errorf("backend WS write failed: %s", err)
		}
	}))
	defer backend.Close()
	target, _ := url.Parse(backend.URL)

	m := newContainerManager(watchdog.NewWatchdog("127.0.0.1:1"), time.Minute)
	e := echo.New()
	real := composeMW(middleware.Rewrite(map[string]string{"/cui/ws*": "/ws$1"}), newCUIProxy(target))
	e.Any("/cui/ws", m.cuiWSHandler(real))

	// Mark the container as running so the WS is proxied for real.
	st := m.states["comfyui"]
	st.mu.Lock()
	st.running = true
	st.mu.Unlock()

	srv := httptest.NewServer(e)
	defer srv.Close()

	ws := dialCUIWS(t, srv)
	defer ws.Close()

	ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	mt, body, err := ws.ReadMessage()
	if err != nil {
		t.Fatalf("expected a proxied message, got error: %s", err)
	}
	if mt != websocket.TextMessage || string(body) != "hello" {
		t.Fatalf("unexpected message: type=%d body=%q", mt, body)
	}
	// The rewrite must forward the container path (/ws), not the raw /cui/ws;
	// otherwise comfyui returns a 404 for the unknown path.
	if p := backendPath.Load().(string); p != "/ws" {
		t.Fatalf("backend received path %q; expected /ws (rewrite dropped the query string)", p)
	}
}

// Once the container starts, the silent WS must close itself so the client
// reconnects to the real proxy.
func TestCUIWSClosesWhenStarted(t *testing.T) {
	m := newContainerManager(watchdog.NewWatchdog("127.0.0.1:1"), time.Minute)
	srv := httptest.NewServer(newCUIWSEcho(m))
	defer srv.Close()

	ws := dialCUIWS(t, srv)
	defer ws.Close()

	// Simulate the container starting (e.g. a REST request triggered it).
	st := m.states["comfyui"]
	st.mu.Lock()
	st.running = true
	st.mu.Unlock()

	// The silent WS polls every silentWSPollInterval and sends a
	// CloseNormalClosure frame on the next poll after the container starts. A
	// deadline expiry (not a close) means it never closed, so assert the exact
	// close frame rather than merely a non-nil error.
	ws.SetReadDeadline(time.Now().Add(5 * silentWSPollInterval))
	_, _, err := ws.ReadMessage()
	if !websocket.IsCloseError(err, websocket.CloseNormalClosure) {
		t.Fatalf("expected the silent WS to close with a close frame, got: %v", err)
	}
}
