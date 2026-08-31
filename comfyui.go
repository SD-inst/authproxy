package main

import (
	"bytes"
	"net/http"
	"net/url"
	"time"

	"github.com/gorilla/websocket"
	"github.com/labstack/echo/v4"
	"github.com/rkfg/authproxy/progress"
	"github.com/rkfg/authproxy/proxy"
	"github.com/rkfg/authproxy/servicequeue"
)

func newCUIProxy(cuiurl *url.URL) echo.MiddlewareFunc {
	return proxy.NewProxyWrapper(cuiurl, nil)
}

func addCUIHandlers(e *echo.Echo, sq *servicequeue.ServiceQueue, cuiurl *url.URL, pr progress.CUIReseter) {
	e.POST("/cui/join", func(c echo.Context) error {
		sq.Lock()
		defer sq.Unlock()
		sq.AwaitReent(servicequeue.CUI)
		sq.CF = &servicequeue.CleanupFunc{
			F: func() {
				sq.SetCleanupProgress(false)
				http.Post(cuiurl.String()+"/free", echo.MIMEApplicationJSON, bytes.NewBufferString(`{"unload_models":"true","free_memory":"true"}`))
				sq.WaitForCleanup(time.Second * 20)
			},
			Service: servicequeue.CUI}
		sq.SetService(servicequeue.CUI, "preparing...")
		// ComfyUI does not send a zero-progress on task start, so reset the
		// previous task's progress and ETA baselines here (otherwise the
		// progress lingers at 100% until the next sampler's first step).
		pr.ResetCUI()
		return nil
	})
	e.POST("/cui/leave", func(c echo.Context) error {
		sq.Lock()
		sq.AwaitReent(servicequeue.CUI)
		sq.SetCleanup(time.Second * 3)
		sq.Unlock()
		return nil
	})
}

// silentWSPollInterval is how often a silent (container-stopped) ComfyUI
// websocket checks whether the container has started, so it can close itself and
// let the client reconnect to the real proxy.
const silentWSPollInterval = 2 * time.Second

// composeMW composes the given middlewares (first = outermost) over a no-op
// terminal into a single handler. The ComfyUI proxy middleware short-circuits
// (it proxies and writes the response itself), so the terminal is never reached
// on a successful proxy.
func composeMW(mws ...echo.MiddlewareFunc) echo.HandlerFunc {
	var h echo.HandlerFunc = func(c echo.Context) error { return nil }
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}

// cuiWSHandler handles the ComfyUI websocket. If the container is running (or
// auto start/stop is disabled) it proxies the connection to the real backend via
// the given handler; if the container is stopped it accepts a silent websocket
// instead — no data is sent and the container is not started. The silent socket
// closes itself once the container starts, so the client reconnects and lands on
// the real proxy. This is what stops an idle tab's websocket reconnect from
// restarting the container.
func (m *containerManager) cuiWSHandler(real echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		if m == nil || !m.enabled() || m.isRunning("comfyui") {
			return real(c)
		}
		return m.silentWS("comfyui", c)
	}
}

// cuiJobsStub is the empty /api/jobs response served while the container is
// stopped, so the web UI's post-reconnect poll sees no work and does not proxy
// (and thereby start) the container.
var cuiJobsStub = map[string]any{
	"jobs": []any{},
	"pagination": map[string]any{
		"offset":   0,
		"limit":    0,
		"total":    0,
		"has_more": false,
	},
}

// cuiJobsHandler handles the ComfyUI /api/jobs poll fired right after a
// websocket reconnect. If the container is running (or auto start/stop is
// disabled) it proxies to the real backend via the given handler; if the
// container is stopped it returns the empty jobs stub instead — no proxy and no
// container start. This keeps the silent-websocket trick effective: without it
// the page's post-reconnect API burst would restart the container.
func (m *containerManager) cuiJobsHandler(real echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		if m == nil || !m.enabled() || m.isRunning("comfyui") {
			return real(c)
		}
		return c.JSON(http.StatusOK, cuiJobsStub)
	}
}

// silentWS upgrades to a websocket that accepts the connection but sends nothing.
// It stays open (holding the client) while the container is stopped and closes as
// soon as the container starts, so the client reconnects to the real proxy. The
// ComfyUI socket is server→client only, so incoming messages are ignored; the
// read loop exists solely to detect when the client goes away.
func (m *containerManager) silentWS(svc string, c echo.Context) error {
	upg := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	conn, err := upg.Upgrade(c.Response(), c.Request(), nil)
	if err != nil {
		return err
	}
	defer conn.Close()
	quit := make(chan struct{})
	go func() {
		// The ComfyUI socket is server→client only, so message contents are
		// ignored; this loop exists solely to detect when the client goes away
		// (ReadMessage errors) so we can stop holding the handler. close(quit)
		// runs exactly once, when this goroutine exits.
		defer close(quit)
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()
	ticker := time.NewTicker(silentWSPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-quit:
			return nil // client went away
		case <-ticker.C:
			if m.isRunning(svc) {
				// Container is up: send a close frame so the client reconnects to
				// the real proxy.
				_ = conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "backend online"))
				return nil
			}
		}
	}
}
