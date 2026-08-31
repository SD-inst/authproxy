package main

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/rkfg/authproxy/watchdog"
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

// managedServices lists the containers the proxy auto-starts on demand and stops
// when idle. These are the compose service names sdwd resolves to containers.
var managedServices = []string{
	"stablediff-cuda", // A1111
	"comfyui",
	"llama-swap", // LLM
	"acestep15",
}

// domainService maps a request-Host prefix (a key of the `domains` map) to the
// managed service it fronts. An absent entry means the domain is not managed.
var domainService = map[string]string{
	"":      "stablediff-cuda", // main domain
	"as15.": "acestep15",
	"cui.":  "comfyui",
}

type svcState struct {
	mu      sync.Mutex
	running bool
	last    time.Time
	timer   *time.Timer
}

// containerManager tracks which managed containers are running, starts them on
// demand, and stops them after stopAfter of inactivity. Right after authproxy
// starts no container is considered running.
type containerManager struct {
	wd        *watchdog.Watchdog
	stopAfter time.Duration
	states    map[string]*svcState
}

// newContainerManager builds a manager for the given watchdog and inactivity
// timeout. If the watchdog has no address, auto start/stop is disabled.
func newContainerManager(wd *watchdog.Watchdog, stopAfter time.Duration) *containerManager {
	m := &containerManager{
		wd:        wd,
		stopAfter: stopAfter,
		states:    make(map[string]*svcState),
	}
	for _, svc := range managedServices {
		m.states[svc] = &svcState{}
	}
	return m
}

func (m *containerManager) enabled() bool {
	return m != nil && m.wd.Enabled()
}

// ensureService returns middleware that makes sure the container backing svc is
// running (starting it synchronously if needed, suspending the request until it
// is ready) and refreshes its inactivity timer both when the request starts and
// when it completes.
func (m *containerManager) ensureService(svc string) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
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
	if !m.enabled() {
		return
	}
	st := m.states[svc]
	st.mu.Lock()
	defer st.mu.Unlock() // held for the whole start so waiters suspend on it
	if st.running {
		return
	}
	if m.startLocked(svc, st) {
		st.running = true
		st.last = time.Now()
		log.Printf("Service %s started", svc)
	}
}

// startLocked runs up to startAttempts start commands for svc, each bounded by
// startAttemptTimeout. It reports whether the container ended up running. Must
// be called with st.mu held.
func (m *containerManager) startLocked(svc string, st *svcState) bool {
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
// st.mu held.
func (m *containerManager) armTimerLocked(svc string, st *svcState) {
	if st.timer != nil {
		st.timer.Stop()
	}
	st.timer = time.AfterFunc(m.stopAfter, func() {
		st.mu.Lock()
		defer st.mu.Unlock()
		// Re-check: a recent access may have re-armed the timer, making this
		// firing stale.
		if st.running && time.Since(st.last) >= m.stopAfter {
			st.running = false
			st.timer = nil
			log.Printf("Service %s idle for %s, stopping", svc, m.stopAfter)
			ctx, cancel := context.WithTimeout(context.Background(), stopAttemptTimeout)
			resp, err := m.wd.Exec(ctx, "stop "+svc)
			cancel()
			if err != nil {
				log.Printf("Error stopping %s: %s", svc, err)
			} else if resp != "ok" {
				log.Printf("Error stopping %s: %s", svc, resp)
			}
		}
	})
}
