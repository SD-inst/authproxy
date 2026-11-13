package main

import (
	"bytes"
	"io"
	"net/http"
	"net/url"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/labstack/echo/v4"
	"github.com/rkfg/authproxy/progress"
	"github.com/rkfg/authproxy/proxy"
	"github.com/rkfg/authproxy/servicequeue"
)

// newCUIProxy builds the ComfyUI proxy with the owner-injection Before hook, so
// the /prompt request carries the X-Authproxy-User header. The same proxy is
// also used for the /cui/ws and /cui/api/jobs routes, where the header is a
// harmless no-op (a task is not created through them).
func newCUIProxy(cuiurl *url.URL) echo.MiddlewareFunc {
	return proxy.NewProxyWrapper(cuiurl, &proxy.Interceptor{Before: injectUser})
}

func addCUIHandlers(e *echo.Echo, sq *servicequeue.ServiceQueue, cuiurl *url.URL, qc progress.QueueControls) {
	e.POST("/cui/join", func(c echo.Context) error {
		user := authUser(c) // the ComfyUI page sends the sdkey cookie
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
		qc.SetTaskColor(taskColor(user))
		// ComfyUI does not send a zero-progress on task start, so reset the
		// previous task's progress and ETA baselines here (otherwise the
		// progress lingers at 100% until the next sampler's first step).
		qc.ResetCUI()
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
		// During an active downtime a stopped container is not served by the
		// silent (fake) websocket either: answer with a 502 so Caddy can show
		// the maintenance/timer page. Mirrors the ensureService guard.
		if m != nil && m.downtime && !m.isRunning(comfyService) {
			return echo.NewHTTPError(http.StatusBadGateway, "service temporarily unavailable")
		}
		if m == nil || !m.enabled() || m.isRunning(comfyService) {
			return real(c)
		}
		return m.silentWS(comfyService, c)
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
		if m == nil || !m.enabled() || m.isRunning(comfyService) {
			return real(c)
		}
		return c.JSON(http.StatusOK, cuiJobsStub)
	}
}

// cuiObjectInfoMaxBody bounds the node-definitions body the proxy will keep.
// The real one is ~12 MB; the bound exists only so a broken upstream cannot grow
// the replay cache without end.
const cuiObjectInfoMaxBody = 64 << 20

// objectInfoSnapshot is one complete /api/object_info body, captured from a
// running ComfyUI so a stopped one can answer with it. boot is the container
// boot the body came from: a snapshot is only replayable within its own boot,
// because a restart is exactly what can change the custom-node set.
type objectInfoSnapshot struct {
	body        []byte
	contentType string
	boot        uint64
}

// objectInfoCache holds the last node-definitions body seen from a running
// ComfyUI container. The frontend refetches /api/object_info on every websocket
// reconnect (useReconnectNodeDefinitionRefresh → fetchServerNodeTypes), and that
// fetch — unlike the /api/jobs poll — has no stub to answer it, so it was what
// woke an idle tab's container.
type objectInfoCache struct {
	cur atomic.Pointer[objectInfoSnapshot]
	// capturing claims the one body copy in flight, so a burst of reconnecting
	// tabs buffers the same 12 MB once instead of once per tab.
	capturing atomic.Bool
}

// snapshot returns the body captured for the given boot, or nil when that boot
// has not produced one yet.
func (c *objectInfoCache) snapshot(boot uint64) *objectInfoSnapshot {
	if snap := c.cur.Load(); snap != nil && snap.boot == boot {
		return snap
	}
	return nil
}

// beginCapture reports whether this response should be copied into the cache,
// claiming the single capture slot when it should. The slot is released by
// finishCapture whether or not the copy completed, so a truncated response never
// wedges the cache.
func (c *objectInfoCache) beginCapture(boot uint64) bool {
	if snap := c.cur.Load(); snap != nil && snap.boot == boot {
		return false
	}
	return c.capturing.CompareAndSwap(false, true)
}

func (c *objectInfoCache) finishCapture() { c.capturing.Store(false) }

// store publishes a complete body for the given boot.
func (c *objectInfoCache) store(body []byte, contentType string, boot uint64) {
	if contentType == "" {
		contentType = echo.MIMEApplicationJSON
	}
	c.cur.Store(&objectInfoSnapshot{body: body, contentType: contentType, boot: boot})
}

// captureHook is the ModifyResponse hook that keeps a copy of a successful
// object_info response. The proxy also calls the interceptor for a failed
// request, where resp is nil.
func (c *objectInfoCache) captureHook(boot func() uint64) func(*http.Request, *http.Response) error {
	return func(_ *http.Request, resp *http.Response) error {
		if resp == nil || resp.Body == nil || resp.StatusCode != http.StatusOK {
			return nil
		}
		b := boot()
		if !c.beginCapture(b) {
			return nil
		}
		resp.Body = &capturingBody{
			Reader: resp.Body,
			closer: resp.Body,
			buf:    &bytes.Buffer{},
			onFinish: func(data []byte, complete bool) {
				defer c.finishCapture()
				if complete {
					c.store(data, resp.Header.Get(echo.HeaderContentType), b)
				}
			},
		}
		return nil
	}
}

// capturingBody streams an upstream body through to the client while keeping a
// copy. The copy is published only if the whole body arrived and ends like a
// finished JSON object: a client that hung up or an upstream that died
// mid-response would otherwise leave the cache holding a truncated catalogue,
// and the frontend reads a valid-but-shorter catalogue as the truth — it skips
// its own full refresh and rebuilds the open graph around the missing nodes.
type capturingBody struct {
	io.Reader
	closer   io.Closer
	buf      *bytes.Buffer
	onFinish func(data []byte, complete bool)
	done     bool
}

func (b *capturingBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	if b.buf != nil {
		if b.buf.Len()+n > cuiObjectInfoMaxBody {
			b.buf = nil // give up on the copy; the client still gets the stream
		} else {
			b.buf.Write(p[:n])
		}
	}
	if err != nil && !b.done {
		b.done = true
		data := []byte(nil)
		complete := false
		if err == io.EOF && b.buf != nil {
			data = b.buf.Bytes()
			complete = len(data) > 0 && data[len(data)-1] == '}'
		}
		b.onFinish(data, complete)
	}
	return n, err
}

func (b *capturingBody) Close() error { return b.closer.Close() }

// newObjectInfoProxy is the object_info proxy: the same owner-injection hook as
// every other ComfyUI route, plus the capture that feeds the replay cache.
func newObjectInfoProxy(cuiurl *url.URL, m *containerManager) echo.MiddlewareFunc {
	return proxy.NewProxyWrapper(cuiurl, &proxy.Interceptor{
		Before: injectUser,
		After:  m.cuiObjectInfo.captureHook(func() uint64 { return m.bootGeneration(comfyService) }),
	})
}

// cuiObjectInfoHandler handles the node-definitions fetch the frontend fires on
// every websocket reconnect. If the container is running (or auto start/stop is
// disabled) it proxies through the given handler. If the container is stopped it
// replays the body captured during this container boot, so an idle tab's
// reconnect neither proxies nor starts ComfyUI. The replay cannot go stale
// across a start: the silent websocket closes the moment the container is up,
// and the reconnect that follows fetches the definitions through the running
// proxy, which captures them for the new boot. With nothing captured for this
// boot there is nothing to replay, and that one case still starts the container
// — the container is the only source of the definitions.
func (m *containerManager) cuiObjectInfoHandler(real echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		if m == nil || !m.enabled() || m.isRunning(comfyService) {
			return real(c)
		}
		if snap := m.cuiObjectInfo.snapshot(m.bootGeneration(comfyService)); snap != nil {
			return c.Blob(http.StatusOK, snap.contentType, snap.body)
		}
		m.ensureRunning(comfyService)
		return real(c)
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
