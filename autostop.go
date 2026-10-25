package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/labstack/echo/v4"
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

// Lifecycle operations accepted by containerManager.control. They are exactly
// the ops the watchdog control protocol understands, and a manual request must
// name one of them verbatim: the op is interpolated into the single-line
// watchdog command, so nothing else may ever reach it.
const (
	opStart   = "start"
	opStop    = "stop"
	opRestart = "restart"
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

type svcState struct {
	mu sync.Mutex
	// idleStop mirrors the managedService policy for this service: the
	// inactivity timer is only ever armed when it is set.
	idleStop bool
	running  bool
	last     time.Time
	timer    *time.Timer
}

// containerManager tracks which managed containers are running, starts them on
// demand, and stops them after stopAfter of inactivity — except for the services
// whose managedService entry turns the inactivity stop off, which stay up until
// stopped explicitly. Right after authproxy starts no container is considered
// running.
type containerManager struct {
	wd        wdClient
	stopAfter time.Duration
	states    map[string]*svcState
	// downtime marks an active maintenance window: stopped containers are never
	// woken up on demand, and their requests get a 502 (which Caddy turns into
	// the maintenance/timer page) instead of a start.
	downtime bool
}

// newContainerManager builds a manager for the given watchdog and inactivity
// timeout. If the watchdog has no address, auto start/stop is disabled.
func newContainerManager(wd wdClient, stopAfter time.Duration) *containerManager {
	m := &containerManager{
		wd:        wd,
		stopAfter: stopAfter,
		states:    make(map[string]*svcState),
	}
	for _, svc := range managedServices {
		m.states[svc.Name] = &svcState{idleStop: svc.IdleStop}
	}
	return m
}

func (m *containerManager) enabled() bool {
	return m != nil && m.wd != nil && m.wd.Enabled()
}

// isRunning reports whether the manager believes svc is currently running. It is
// the manager's own view of the lifecycle (it assumes nothing is running at
// startup and only marks a service running once it has successfully started it),
// which is the same view the auto start/stop relies on.
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
	if m.startLocked(svc) {
		st.running = true
		st.last = time.Now()
		log.Printf("Service %s started", svc)
	}
}

// startLocked runs up to startAttempts start commands for svc, each bounded by
// startAttemptTimeout. It reports whether the container ended up running. Must
// be called with the service lock (svcState.mu) held.
func (m *containerManager) startLocked(svc string) bool {
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
		if !m.startLocked(svc) {
			return fmt.Errorf("service %s failed to start", svc)
		}
		st.running = true
		st.last = time.Now()
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
		if err != nil {
			log.Printf("Error stopping %s: %s", svc, err)
			return fmt.Errorf("error stopping %s: %w", svc, err)
		}
		log.Printf("Service %s stopped manually", svc)
	case opRestart:
		// The watchdog waits for the container to become healthy, so this needs
		// the start budget, not the stop one.
		ctx, cancel := context.WithTimeout(context.Background(), startAttemptTimeout)
		defer cancel()
		resp, err := m.wd.Exec(ctx, "restart "+svc)
		if err == nil && resp != "ok" {
			err = errors.New(resp)
		}
		if err != nil {
			log.Printf("Error restarting %s: %s", svc, err)
			return fmt.Errorf("error restarting %s: %w", svc, err)
		}
		st.running = true
		st.last = time.Now()
		m.armTimerLocked(svc, st)
		log.Printf("Service %s restarted manually", svc)
	}
	return nil
}

// containerState is one tracked service as the manager sees it. The view is the
// manager's own lifecycle state (the same one auto start/stop relies on), not a
// live probe of Docker.
type containerState struct {
	Service string `json:"service"`
	Running bool   `json:"running"`
	// LastActivity is when the service was last started or served a request
	// (RFC3339). Empty while the service has never been started.
	LastActivity string `json:"last_activity,omitempty"`
	// IdleStop reports whether the inactivity stop applies to this service at
	// all. A service with it off stays running until stopped explicitly.
	IdleStop bool `json:"idle_stop"`
	// IdleStopIn is how long until the inactivity stop fires, as a Go duration.
	// Only set for a running service that has the inactivity stop.
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
}

// status snapshots the manager's view of every tracked service, in the order of
// managedServices.
func (m *containerManager) status() containerStatus {
	s := containerStatus{
		AutoControl: m.enabled(),
		Downtime:    m.downtime,
		StopAfter:   m.stopAfter.String(),
		Services:    make([]containerState, 0, len(m.states)),
	}
	for _, svc := range managedServices {
		st := m.states[svc.Name]
		st.mu.Lock()
		cs := containerState{Service: svc.Name, Running: st.running, IdleStop: st.idleStop}
		if st.running {
			cs.LastActivity = st.last.Format(time.RFC3339)
			if st.idleStop {
				if left := m.stopAfter - time.Since(st.last); left > 0 {
					cs.IdleStopIn = left.Round(time.Second).String()
				}
			}
		}
		st.mu.Unlock()
		s.Services = append(s.Services, cs)
	}
	return s
}
