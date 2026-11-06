package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/rkfg/authproxy/servicequeue"
)

// startAttemptTimeout bounds each individual start attempt; startAttempts is
// the total number of attempts (initial + retries) before giving up and letting
// the request through. Together they implement "wait up to a minute, retry
// twice more, then give up".
const (
	startAttemptTimeout = 1 * time.Minute
	startAttempts       = 3
	// stopAttemptTimeout bounds how long a stop command may take (docker's
	// default 10 s stop grace plus headroom) while the service lock is held.
	stopAttemptTimeout = 30 * time.Second
)

// wdClient is the control-endpoint surface the manager needs. Declared as an
// interface so tests can substitute a fake; *watchdog.Watchdog is the only
// implementation used in production.
type wdClient interface {
	Enabled() bool
	Exec(ctx context.Context, command string) (string, error)
}

// Lifecycle operations accepted by containerManager.control. The control
// endpoint also answers a read-only "status" query, which only the reconciler
// sends; a manual request must name one of these three verbatim, because the op
// is interpolated into the single-line watchdog command and nothing else may
// ever reach it.
const (
	opStart   = "start"
	opStop    = "stop"
	opRestart = "restart"
)

// Container states the manager tracks and reports. The watchdog's "status" op
// answers with one of these words, so the same vocabulary means both the
// manager's belief and what Docker was last seen saying.
const (
	stateStopped = "stopped"
	// stateStarting covers a start the manager itself is waiting for and a
	// container Docker reports up but not yet passing its healthcheck. It is a
	// distinct state because a container stuck starting (an OOM restart loop,
	// say) must be visible as such rather than hide the whole status page.
	stateStarting  = "starting"
	stateRunning   = "running"
	stateUnhealthy = "unhealthy"
)

// Compose service names of the containers the proxy tracks. They are the names
// sdwd resolves to containers and the names the admin page lists.
const (
	sdService      = "stablediff-cuda" // A1111
	comfyService   = "comfyui"
	acestepService = "acestep15"
	llmService     = "llama-swap"
)

// managedService is one tracked container: it is started on demand when a request
// arrives for it, and, unless IdleStop is set, stopped after the inactivity
// timeout.
type managedService struct {
	Name string
	// IdleStop turns the inactivity stop on for this container. The LLM proxy
	// keeps its container running once it has been started: llama-swap does its
	// own model swapping, and its usefulness is in staying loaded, so only an
	// explicit stop (the admin page) takes it down.
	IdleStop bool
}

var managedServices = []managedService{
	{Name: sdService, IdleStop: true},
	{Name: comfyService, IdleStop: true},
	{Name: acestepService, IdleStop: true},
	{Name: llmService, IdleStop: false},
}

// domainService maps a request-Host prefix (a key of the `domains` map) to the
// managed service it fronts. An absent entry means the domain is not managed.
var domainService = map[string]string{
	"":      sdService, // main domain
	"as15.": acestepService,
	"cui.":  comfyService,
}

// gpuSlot maps a managed container to the queue slot it claims across its start.
// Booting the container is what touches the GPU — the process initializes CUDA
// and loads what it loads as it comes up — so the start must not overlap the
// service that currently owns the slot with a model resident in VRAM: claiming
// the slot first runs that service's cleanup, which is what frees the memory.
// The LLM container is deliberately absent: llama-swap allocates nothing when it
// starts (the model loads on the request, under the queue), so gating its start
// would only make LLM users wait behind one another for nothing. The slot named
// here is the one that service's own join handler re-enters, which is why the
// acestep15 container maps to ACESTEP15 and not to the older ACESTEP slot.
var gpuSlot = map[string]servicequeue.SvcType{
	sdService:      servicequeue.SD,
	comfyService:   servicequeue.CUI,
	acestepService: servicequeue.ACESTEP15,
}

// pubState is the lock-free view of one service, replaced whole whenever the
// guarded state changes.
type pubState struct {
	state string
	last  time.Time
}

type svcState struct {
	mu sync.Mutex
	// idleStop mirrors the managedService policy for this service: the
	// inactivity timer is only ever armed when it is set. It is fixed at
	// construction, so it may be read without the lock.
	idleStop bool
	running  bool
	// state is the display state: running plus the health the watchdog last
	// reported, or starting while a start is in flight. Guarded by mu, and
	// mirrored into pub by publishLocked.
	state string
	last  time.Time
	timer *time.Timer
	// pub is the last published snapshot. status() reads it instead of the
	// guarded fields: an op in flight holds mu for as long as a start can take
	// (up to startAttempts × startAttemptTimeout), and the admin page must keep
	// answering through that instead of blocking behind one sick container.
	pub atomic.Pointer[pubState]
}

// publishLocked mirrors the guarded state into the lock-free snapshot. Must be
// called with mu held, after every change to state or last.
func (st *svcState) publishLocked() {
	st.pub.Store(&pubState{state: st.state, last: st.last})
}

// setPhaseLocked changes the display state and republishes it. Must be called
// with mu held.
func (st *svcState) setPhaseLocked(state string) {
	st.state = state
	st.publishLocked()
}

// settleLocked restores the display state implied by the running belief, for the
// case where an op failed and nothing better is known. The reconciler replaces
// it with what Docker says on the next pass. Must be called with mu held.
func (st *svcState) settleLocked() {
	if st.running {
		st.setPhaseLocked(stateRunning)
	} else {
		st.setPhaseLocked(stateStopped)
	}
}

// containerManager tracks which managed containers are running, starts them on
// demand, and stops them after stopAfter of inactivity — except for the services
// whose managedService entry turns the inactivity stop off, which stay up until
// stopped explicitly. The view is kept in step with the containers that actually
// exist by startReconciler, so a container that was already up when authproxy
// started — the usual case when only authproxy is restarted — is tracked as
// running and gets its inactivity timer armed.
type containerManager struct {
	wd        wdClient
	stopAfter time.Duration
	states    map[string]*svcState
	// sq is the GPU slot the manager holds across a container start, so a
	// container's CUDA init never overlaps a service that has a model resident.
	// A manager built without it starts containers ungated, which is what the
	// tests that only exercise the lifecycle view do.
	sq *servicequeue.ServiceQueue
	// downtime marks an active maintenance window: stopped containers are never
	// woken up on demand, and their requests get a 502 (which Caddy turns into
	// the maintenance/timer page) instead of a start. It is set once at startup
	// before the handlers are registered.
	downtime bool
	// recon reports when the view was last reconciled with the control endpoint
	// and what failed, if anything. Written by the reconciler, read by status.
	recon atomic.Pointer[reconcileInfo]
}

// reconcileInfo is the freshness of the manager's view, for the admin page.
type reconcileInfo struct {
	at  time.Time
	err string
}

// newContainerManager builds a manager for the given watchdog, inactivity
// timeout, and GPU slot. If the watchdog has no address, auto start/stop is
// disabled.
func newContainerManager(wd wdClient, stopAfter time.Duration, sq *servicequeue.ServiceQueue) *containerManager {
	m := &containerManager{
		wd:        wd,
		stopAfter: stopAfter,
		sq:        sq,
		states:    make(map[string]*svcState),
	}
	for _, svc := range managedServices {
		st := &svcState{idleStop: svc.IdleStop, state: stateStopped}
		st.publishLocked()
		m.states[svc.Name] = st
	}
	return m
}

func (m *containerManager) enabled() bool {
	return m != nil && m.wd != nil && m.wd.Enabled()
}

// isRunning reports whether the manager believes svc is currently running. It is
// the manager's own view of the lifecycle — adopted from the control endpoint at
// startup and kept in step by the reconciler, and updated by every op it
// performs — which is the same view the auto start/stop relies on.
func (m *containerManager) isRunning(svc string) bool {
	st := m.states[svc]
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.running
}

// ensureService returns middleware that makes sure the container backing svc is
// running (starting it synchronously if needed, suspending the request until it
// is ready) and refreshes its inactivity timer both when the request starts and
// when it completes. During an active downtime a not-yet-running container is
// not started; the request is answered with a 502 instead, so Caddy can serve
// the maintenance/timer page.
func (m *containerManager) ensureService(svc string) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if m.downtime && !m.isRunning(svc) {
				return echo.NewHTTPError(http.StatusBadGateway, "service temporarily unavailable")
			}
			m.ensureRunning(svc)
			defer m.touch(svc) // keep the timer alive once the request is done
			m.touch(svc)
			return next(c)
		}
	}
}

// withDomain wraps a domain proxy middleware so that, if the domain is managed,
// the backing service is ensured running and its idle timer is refreshed first.
func (m *containerManager) withDomain(tw echo.MiddlewareFunc, d string) echo.MiddlewareFunc {
	if svc, ok := domainService[d]; ok {
		return func(next echo.HandlerFunc) echo.HandlerFunc {
			return m.ensureService(svc)(tw(next))
		}
	}
	return tw
}

// ensureRunning starts svc if it is not running and blocks until it is ready.
// Concurrent callers for the same service share a single start: the first one
// performs it (holding the service lock) while the rest block on the lock and
// proceed once it has finished.
func (m *containerManager) ensureRunning(svc string) {
	if !m.enabled() || m.downtime {
		return
	}
	st := m.states[svc]
	st.mu.Lock()
	defer st.mu.Unlock() // held for the whole start so waiters suspend on it
	if st.running {
		return
	}
	// Visible to the admin page while the lock is held: a container that never
	// comes up keeps every request parked on this lock, and that must show as a
	// starting container rather than as a frozen status page.
	st.setPhaseLocked(stateStarting)
	if m.startLocked(svc) {
		st.running = true
		st.last = time.Now()
		st.setPhaseLocked(stateRunning)
		log.Printf("Service %s started", svc)
		return
	}
	st.setPhaseLocked(stateStopped)
}

// claimGPUSlot takes the queue slot of svc before its container is started, and
// reports whether it claimed one. Waiting for the slot is what evicts the model
// resident on the GPU: the queue runs the previous holder's cleanup on the
// transition, and that is the whole point — the memory has to be gone before
// this container's process starts initializing CUDA. A service that already owns
// the slot re-enters it, which disturbs nothing: the container is up in that
// case, and a start is only an idempotent re-check. Must be called with the
// service lock held, which is what keeps two services from starting at once.
func (m *containerManager) claimGPUSlot(svc string) bool {
	t, ok := gpuSlot[svc]
	if !ok || m.sq == nil {
		return false
	}
	m.sq.Lock()
	defer m.sq.Unlock()
	m.sq.AwaitReent(t)
	// AwaitReent names the holder; the description is what the status page shows
	// while the container boots.
	m.sq.SetService(t, "starting...")
	return true
}

// releaseGPUSlot gives up the slot taken by claimGPUSlot once the start is over.
// The slot is freed rather than kept for the container's warm-up, and that is a
// deliberate trade: the join handlers decide by the queue transition whether the
// service has to load what it loads on demand — A1111 posts reload-checkpoint
// only when it has just taken the slot — so a slot still claimed by the manager
// would read to them as a service that is already loaded. What is left open is
// only the second or two between the container answering its healthcheck and
// finishing its own GPU init, which is far narrower than the window this claim
// closes, where the previous model was resident for the whole boot.
func (m *containerManager) releaseGPUSlot(svc string) {
	t, ok := gpuSlot[svc]
	if !ok || m.sq == nil {
		return
	}
	m.sq.Lock()
	defer m.sq.Unlock()
	if m.sq.Service() != t {
		// A stale cleanup timer zeroed the slot during the start and someone else
		// has since taken it. Freeing it here would hand out their hold.
		return
	}
	m.sq.SetService(servicequeue.NONE)
}

// startLocked claims the GPU slot for svc, starts its container, and gives the
// slot back. It reports whether the container ended up running. Must be called
// with the service lock (svcState.mu) held. The queue lock is taken inside, and
// always after the service lock: nothing that holds the queue lock reaches back
// into the manager, so the two can never be held in the opposite order.
func (m *containerManager) startLocked(svc string) bool {
	if !m.claimGPUSlot(svc) {
		return m.startAttemptsLocked(svc)
	}
	started := m.startAttemptsLocked(svc)
	m.releaseGPUSlot(svc)
	return started
}

// startAttemptsLocked runs up to startAttempts start commands for svc, each
// bounded by startAttemptTimeout. It reports whether the container ended up
// running. Must be called with the service lock held.
func (m *containerManager) startAttemptsLocked(svc string) bool {
	for attempt := 1; attempt <= startAttempts; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), startAttemptTimeout)
		resp, err := m.wd.Exec(ctx, "start "+svc)
		cancel()
		switch {
		case err != nil:
			log.Printf("Service %s start (attempt %d/%d): %s", svc, attempt, startAttempts, err)
		case resp == "ok":
			return true
		default:
			log.Printf("Service %s start (attempt %d/%d): %s", svc, attempt, startAttempts, resp)
		}
	}
	log.Printf("Service %s failed to start after %d attempts", svc, startAttempts)
	return false
}

// touch refreshes the inactivity timer for a running service.
func (m *containerManager) touch(svc string) {
	st := m.states[svc]
	st.mu.Lock()
	defer st.mu.Unlock()
	if !st.running {
		return
	}
	st.last = time.Now()
	st.publishLocked()
	m.armTimerLocked(svc, st)
}

// armTimerLocked (re)schedules the idle-stop for svc. Must be called with
// st.mu held. A service whose policy turns the inactivity stop off never gets a
// timer, no matter what started it — a request or the admin page.
func (m *containerManager) armTimerLocked(svc string, st *svcState) {
	if !st.idleStop {
		return
	}
	if st.timer != nil {
		st.timer.Stop()
	}
	st.timer = time.AfterFunc(m.stopAfter, func() {
		st.mu.Lock()
		// Re-check: a recent access may have re-armed the timer, making this
		// firing stale.
		if !(st.running && time.Since(st.last) >= m.stopAfter) {
			st.mu.Unlock()
			return
		}
		st.running = false
		st.timer = nil
		st.setPhaseLocked(stateStopped)
		log.Printf("Service %s idle for %s, stopping", svc, m.stopAfter)
		// Hold the lock across the stop. sdwd's start inspects the container
		// before acting and is idempotent; a container mid-teardown still
		// reports running and healthy, so a start issued while the stop is in
		// flight is a no-op that returns "ok" — leaving the proxy convinced the
		// service is up when it is actually down and never restarted. Keeping
		// the stop under the lock serializes the two ops, so a restarted service
		// waits for the stop to finish before the start runs.
		ctx, cancel := context.WithTimeout(context.Background(), stopAttemptTimeout)
		resp, err := m.wd.Exec(ctx, "stop "+svc)
		cancel()
		if err != nil {
			log.Printf("Error stopping %s: %s", svc, err)
		} else if resp != "ok" {
			log.Printf("Error stopping %s: %s", svc, resp)
		}
		st.mu.Unlock()
	})
}

// control applies a manually requested lifecycle op to a tracked service. It is
// the manual counterpart of the automatic start/stop and takes the same service
// lock, so a manual op can never interleave with an automatic one, with a
// request that would wake the container, or with the idle-stop timer.
//
// The ops are idempotent because the watchdog inspects the container before
// acting: "start" on a service the manager believes is running is a cheap
// re-check that leaves a healthy container untouched and revives one that the
// manager's view has drifted away from. A started or restarted service gets its
// inactivity timer armed, exactly as a request-driven start does.
//
// A failed "stop" still marks the service not running: the container is then
// unknown to us, and the next request issues an idempotent start, which is the
// self-healing path. The error is reported to the caller either way.
//
// While an op is in flight the service is reported as starting, so the admin
// page shows what is happening instead of appearing frozen behind the lock.
//
// The downtime flag does not gate manual ops — an operator using this endpoint
// is deliberately overriding maintenance mode.
func (m *containerManager) control(svc, op string) error {
	if !m.enabled() {
		return errors.New("container control is disabled: no watchdog address configured")
	}
	st, ok := m.states[svc]
	if !ok {
		return fmt.Errorf("unknown service %q", svc)
	}
	switch op {
	case opStart, opStop, opRestart:
	default:
		return fmt.Errorf("unknown op %q", op)
	}

	st.mu.Lock()
	defer st.mu.Unlock()

	switch op {
	case opStart:
		st.setPhaseLocked(stateStarting)
		if !m.startLocked(svc) {
			st.settleLocked()
			return fmt.Errorf("service %s failed to start", svc)
		}
		st.running = true
		st.last = time.Now()
		st.setPhaseLocked(stateRunning)
		m.armTimerLocked(svc, st)
		log.Printf("Service %s started manually", svc)
	case opStop:
		ctx, cancel := context.WithTimeout(context.Background(), stopAttemptTimeout)
		defer cancel()
		resp, err := m.wd.Exec(ctx, "stop "+svc)
		if err == nil && resp != "ok" {
			err = errors.New(resp)
		}
		st.running = false
		if st.timer != nil {
			st.timer.Stop()
			st.timer = nil
		}
		st.setPhaseLocked(stateStopped)
		if err != nil {
			log.Printf("Error stopping %s: %s", svc, err)
			return fmt.Errorf("error stopping %s: %w", svc, err)
		}
		log.Printf("Service %s stopped manually", svc)
	case opRestart:
		// The watchdog waits for the container to become healthy, so this needs
		// the start budget, not the stop one. A restart re-allocates the
		// container's GPU memory, so it needs the slot for the same reason a
		// start does, and the slot is given up afterwards: the container came back
		// with nothing loaded, and its clients reconnect through the join handlers,
		// which reload on a freshly taken slot.
		ctx, cancel := context.WithTimeout(context.Background(), startAttemptTimeout)
		defer cancel()
		st.setPhaseLocked(stateStarting)
		claimed := m.claimGPUSlot(svc)
		resp, err := m.wd.Exec(ctx, "restart "+svc)
		if claimed {
			m.releaseGPUSlot(svc)
		}
		if err == nil && resp != "ok" {
			err = errors.New(resp)
		}
		if err != nil {
			log.Printf("Error restarting %s: %s", svc, err)
			st.settleLocked()
			return fmt.Errorf("error restarting %s: %w", svc, err)
		}
		st.running = true
		st.last = time.Now()
		st.setPhaseLocked(stateRunning)
		m.armTimerLocked(svc, st)
		log.Printf("Service %s restarted manually", svc)
	}
	return nil
}

// reconcileInterval is how often the manager re-reads the real container states,
// and reconcileTimeout bounds one status query. The query is a plain inspect, so
// the timeout is short: a stalled control endpoint must not stall the loop, and
// must never be what the status page waits behind.
const (
	reconcileInterval = 30 * time.Second
	reconcileTimeout  = 5 * time.Second
)

// startReconciler brings the manager's view in line with the containers that
// actually exist: once straight away, then on a ticker. The first pass is what
// makes an authproxy-only restart safe — containers it did not start are adopted
// instead of assumed stopped, which is otherwise what leaves a live container
// with no inactivity timer at all, and so never stopped again. The later passes
// catch the reverse drift: a container that died, was stopped by hand, or was
// recreated by the watchdog while the manager still believes it is up. It does
// nothing without a control endpoint.
func (m *containerManager) startReconciler(ctx context.Context) {
	if !m.enabled() {
		return
	}
	go func() {
		m.reconcile(ctx)
		t := time.NewTicker(reconcileInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				m.reconcile(ctx)
			}
		}
	}()
}

// reconcile queries every tracked container and adopts what the control endpoint
// reports. A failure is recorded, not guessed at: the states keep showing the
// last thing known, and the admin page says the view may be stale.
func (m *containerManager) reconcile(ctx context.Context) {
	var failed []string
	for _, svc := range managedServices {
		if err := m.reconcileService(ctx, svc.Name); err != nil {
			failed = append(failed, svc.Name+": "+err.Error())
		}
	}
	info := &reconcileInfo{at: time.Now()}
	if len(failed) > 0 {
		info.err = strings.Join(failed, "; ")
	}
	// Report a standing problem once rather than on every interval, and say when
	// it is over.
	prev := m.recon.Load()
	switch {
	case info.err != "" && (prev == nil || prev.err != info.err):
		log.Printf("Container status check failed: %s", info.err)
	case info.err == "" && prev != nil && prev.err != "":
		log.Printf("Container status check restored")
	}
	m.recon.Store(info)
}

// reconcileService syncs one service with what the control endpoint says about
// it. It never starts or stops anything: it adopts the state, and that adoption
// is what gives a container the manager did not start its inactivity timer.
func (m *containerManager) reconcileService(ctx context.Context, svc string) error {
	st := m.states[svc]
	if !st.mu.TryLock() {
		// A lifecycle op is in flight for this service, so its own result is the
		// fresher answer and there is nothing to adopt.
		return nil
	}
	defer st.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, reconcileTimeout)
	defer cancel()
	resp, err := m.wd.Exec(ctx, "status "+svc)
	if err != nil {
		return err
	}
	state, err := parseStatusReply(svc, resp)
	if err != nil {
		return err
	}

	if state == stateStopped {
		if st.running {
			log.Printf("Service %s is not running any more, adopting the stopped state", svc)
			st.running = false
			if st.timer != nil {
				st.timer.Stop()
				st.timer = nil
			}
		}
		st.setPhaseLocked(stateStopped)
		return nil
	}

	if !st.running {
		log.Printf("Service %s is already running, adopting it", svc)
		st.running = true
		// last starts now: the manager has no history of this container before
		// this moment, so it gets a full inactivity window instead of being
		// stopped for idleness it cannot account for.
		st.last = time.Now()
		m.armTimerLocked(svc, st)
	}
	st.setPhaseLocked(state)
	return nil
}

// parseStatusReply turns the answer to "status <svc>" into a container state. A
// service with no container in the compose project counts as stopped. Any other
// answer — including "error: unknown op status" from an endpoint too old to
// answer — is reported as an error, so the caller keeps the belief it has
// instead of guessing.
func parseStatusReply(svc, resp string) (string, error) {
	if resp == "error: no containers found for service "+svc {
		return stateStopped, nil
	}
	state, found := strings.CutPrefix(resp, "ok ")
	if !found {
		return "", errors.New(resp)
	}
	switch state {
	case stateStopped, stateStarting, stateRunning, stateUnhealthy:
		return state, nil
	}
	return "", fmt.Errorf("unexpected status reply %q", resp)
}

// containerState is one tracked service as the manager sees it. The view is the
// manager's own lifecycle state — adopted from the control endpoint and updated
// by every op it performs — and it is always served from the published
// snapshot, so it answers while an op is still in flight.
type containerState struct {
	Service string `json:"service"`
	// State is what the manager last saw: stopped, starting, running or
	// unhealthy. Starting covers both a start the manager is waiting for and a
	// container Docker reports up but not yet passing its healthcheck.
	State string `json:"state"`
	// Running is State != stopped: the belief the auto start/stop acts on.
	Running bool `json:"running"`
	// LastActivity is when the service was last started, adopted by the
	// reconciler, or served a request (RFC3339). Empty while none of that has
	// ever happened; an adopted container starts its history at the adoption.
	LastActivity string `json:"last_activity,omitempty"`
	// IdleStop reports whether the inactivity stop applies to this service at
	// all. A service with it off stays running until stopped explicitly.
	IdleStop bool `json:"idle_stop"`
	// IdleStopIn is how long until the inactivity stop fires, as a Go duration.
	// Only set for a service that is not stopped and has the inactivity stop.
	IdleStopIn string `json:"idle_stop_in,omitempty"`
}

// containerStatus is the manager's whole view, for the admin page.
type containerStatus struct {
	// AutoControl reports whether auto start/stop is on (a watchdog address is
	// configured). Manual control needs it too.
	AutoControl bool `json:"auto_control"`
	// Downtime reports the active maintenance window: not-running services are
	// not woken by requests and answer 502 instead.
	Downtime  bool             `json:"downtime"`
	StopAfter string           `json:"stop_after"`
	Services  []containerState `json:"services"`
	// ReconciledAt is when the states were last checked against the control
	// endpoint (RFC3339). Empty while no check has run.
	ReconciledAt string `json:"reconciled_at,omitempty"`
	// ReconcileError says why the last check failed. The states still show the
	// last thing known rather than a guess, so the page can mark them stale.
	ReconcileError string `json:"reconcile_error,omitempty"`
}

// status snapshots the manager's view of every tracked service, in the order of
// managedServices. It reads the published snapshots rather than the guarded
// fields and never waits on a service lock, so one container stuck starting
// cannot take the whole page down with it.
func (m *containerManager) status() containerStatus {
	s := containerStatus{
		AutoControl: m.enabled(),
		Downtime:    m.downtime,
		StopAfter:   m.stopAfter.String(),
		Services:    make([]containerState, 0, len(m.states)),
	}
	if info := m.recon.Load(); info != nil {
		if !info.at.IsZero() {
			s.ReconciledAt = info.at.Format(time.RFC3339)
		}
		s.ReconcileError = info.err
	}
	for _, svc := range managedServices {
		st := m.states[svc.Name]
		pub := st.pub.Load()
		if pub == nil {
			pub = &pubState{state: stateStopped}
		}
		cs := containerState{
			Service:  svc.Name,
			State:    pub.state,
			Running:  pub.state != stateStopped,
			IdleStop: st.idleStop, // fixed at construction, safe without the lock
		}
		if !pub.last.IsZero() {
			cs.LastActivity = pub.last.Format(time.RFC3339)
			if st.idleStop && pub.state != stateStopped {
				if left := m.stopAfter - time.Since(pub.last); left > 0 {
					cs.IdleStopIn = left.Round(time.Second).String()
				}
			}
		}
		s.Services = append(s.Services, cs)
	}
	return s
}
