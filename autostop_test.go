package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

// fakeWd records the commands the manager sends to the control endpoint and
// replays a canned reply, so lifecycle logic can be tested without a watchdog.
type fakeWd struct {
	commands []string
	resp     string
	err      error
	enabled  bool
	// byCommand answers specific commands (the status queries) and falls back to
	// resp for everything else.
	byCommand map[string]string
	// gate, when set, holds every command until it is closed, so an op can be
	// observed while it still owns the service lock. entered carries each command
	// as it reaches the gate.
	gate    chan struct{}
	entered chan string
}

func (f *fakeWd) Enabled() bool { return f.enabled }

func (f *fakeWd) Exec(_ context.Context, command string) (string, error) {
	f.commands = append(f.commands, command)
	if f.gate != nil {
		f.entered <- command
		<-f.gate
	}
	if reply, ok := f.byCommand[command]; ok {
		return reply, f.err
	}
	return f.resp, f.err
}

func newTestManager(wd wdClient) *containerManager {
	return newContainerManager(wd, 60*time.Minute)
}

// statusWithin takes the status snapshot or fails the test. A status that waits
// behind a service lock is the bug this guards: one container stuck starting used
// to take the whole admin page down with it.
func statusWithin(t *testing.T, m *containerManager, d time.Duration) containerStatus {
	t.Helper()
	ch := make(chan containerStatus, 1)
	go func() { ch <- m.status() }()
	select {
	case s := <-ch:
		return s
	case <-time.After(d):
		t.Fatalf("status() did not answer within %s", d)
		return containerStatus{}
	}
}

// serviceOf picks one service out of a status snapshot.
func serviceOf(s containerStatus, svc string) *containerState {
	for i := range s.Services {
		if s.Services[i].Service == svc {
			return &s.Services[i]
		}
	}
	return nil
}

func TestControlRejectsUnknownService(t *testing.T) {
	wd := &fakeWd{enabled: true, resp: "ok"}
	m := newTestManager(wd)
	if err := m.control("rm -rf /", opStop); err == nil {
		t.Fatal("expected an error for an untracked service")
	}
	if len(wd.commands) != 0 {
		t.Fatal("nothing must reach the watchdog for an untracked service")
	}
}

func TestControlRejectsUnknownOp(t *testing.T) {
	wd := &fakeWd{enabled: true, resp: "ok"}
	m := newTestManager(wd)
	for _, op := range []string{"", "shutdown", "stop ; start comfyui", "STOP"} {
		if err := m.control("comfyui", op); err == nil {
			t.Fatalf("expected an error for op %q", op)
		}
	}
	if len(wd.commands) != 0 {
		t.Fatalf("watchdog must not be called, got %v", wd.commands)
	}
}

func TestControlNeedsWatchdog(t *testing.T) {
	m := newTestManager(&fakeWd{enabled: false, resp: "ok"})
	if err := m.control("comfyui", opStop); err == nil {
		t.Fatal("expected an error when the control endpoint is not configured")
	}
}

func TestControlStart(t *testing.T) {
	wd := &fakeWd{enabled: true, resp: "ok"}
	m := newTestManager(wd)
	if err := m.control("comfyui", opStart); err != nil {
		t.Fatalf("start: %s", err)
	}
	want := []string{"start comfyui"}
	if len(wd.commands) != 1 || wd.commands[0] != want[0] {
		t.Fatalf("commands = %v, want %v", wd.commands, want)
	}
	st := m.states["comfyui"]
	st.mu.Lock()
	running, armed := st.running, st.timer != nil
	st.mu.Unlock()
	if !running {
		t.Fatal("comfyui must be marked running after a manual start")
	}
	// A manually started container must get the inactivity timer armed, or it
	// would never be stopped again: no request will ever touch it.
	if !armed {
		t.Fatal("the idle-stop timer must be armed by a manual start")
	}
}

func TestControlStartFailureLeavesStateStopped(t *testing.T) {
	wd := &fakeWd{enabled: true, resp: "error: no containers found for service comfyui"}
	m := newTestManager(wd)
	if err := m.control("comfyui", opStart); err == nil {
		t.Fatal("expected an error when the start never succeeds")
	}
	st := m.states["comfyui"]
	st.mu.Lock()
	running, commands := st.running, wd.commands
	st.mu.Unlock()
	if running {
		t.Fatal("a failed start must not mark the service running")
	}
	// startLocked retries startAttempts times before giving up.
	if len(commands) != startAttempts {
		t.Fatalf("expected %d start attempts, got %d", startAttempts, len(commands))
	}
}

func TestControlStopClearsStateAndTimer(t *testing.T) {
	wd := &fakeWd{enabled: true, resp: "ok"}
	m := newTestManager(wd)
	if err := m.control("comfyui", opStart); err != nil {
		t.Fatalf("start: %s", err)
	}
	if err := m.control("comfyui", opStop); err != nil {
		t.Fatalf("stop: %s", err)
	}
	if wd.commands[len(wd.commands)-1] != "stop comfyui" {
		t.Fatalf("commands = %v, want the last one to be %q", wd.commands, "stop comfyui")
	}
	st := m.states["comfyui"]
	st.mu.Lock()
	running, timer := st.running, st.timer
	st.mu.Unlock()
	if running {
		t.Fatal("comfyui must be marked stopped after a manual stop")
	}
	if timer != nil {
		t.Fatal("the idle-stop timer must not be armed for a stopped service")
	}
}

// A failed stop still clears the state: the container is then unknown to us, and
// the next request issues an idempotent start.
func TestControlStopFailureStillClearsState(t *testing.T) {
	wd := &fakeWd{enabled: true, resp: "ok"}
	m := newTestManager(wd)
	if err := m.control("comfyui", opStart); err != nil {
		t.Fatalf("start: %s", err)
	}
	wd.resp, wd.err = "", context.DeadlineExceeded
	if err := m.control("comfyui", opStop); err == nil {
		t.Fatal("the operator must see that the stop command failed")
	}
	st := m.states["comfyui"]
	st.mu.Lock()
	running := st.running
	st.mu.Unlock()
	if running {
		t.Fatal("a service whose stop failed must not be left believed running")
	}
}

func TestControlRestart(t *testing.T) {
	wd := &fakeWd{enabled: true, resp: "ok"}
	m := newTestManager(wd)
	if err := m.control("comfyui", opRestart); err != nil {
		t.Fatalf("restart: %s", err)
	}
	if wd.commands[0] != "restart comfyui" {
		t.Fatalf("commands = %v", wd.commands)
	}
	st := m.states["comfyui"]
	st.mu.Lock()
	running, armed := st.running, st.timer != nil
	st.mu.Unlock()
	if !running || !armed {
		t.Fatalf("after a restart the service must be running (running=%v armed=%v)", running, armed)
	}
}

func TestStatusSnapshot(t *testing.T) {
	wd := &fakeWd{enabled: true, resp: "ok"}
	m := newTestManager(wd)
	s := m.status()
	if !s.AutoControl {
		t.Fatal("AutoControl must report the configured watchdog")
	}
	if s.StopAfter != (60 * time.Minute).String() {
		t.Fatalf("StopAfter = %q", s.StopAfter)
	}
	if len(s.Services) != len(managedServices) {
		t.Fatalf("got %d services, want %d", len(s.Services), len(managedServices))
	}
	for _, svc := range s.Services {
		if svc.Running {
			t.Fatalf("service %s must start out stopped", svc.Service)
		}
	}

	if err := m.control("comfyui", opStart); err != nil {
		t.Fatalf("start: %s", err)
	}
	s = m.status()
	var comfy *containerState
	for i, svc := range s.Services {
		if svc.Service == "comfyui" {
			comfy = &s.Services[i]
		}
	}
	if comfy == nil || !comfy.Running {
		t.Fatal("comfyui must be reported running")
	}
	if comfy.LastActivity == "" {
		t.Fatal("a running service must report its last activity")
	}
	if _, err := time.ParseDuration(comfy.IdleStopIn); err != nil {
		t.Fatalf("IdleStopIn %q is not a duration: %s", comfy.IdleStopIn, err)
	}
}

// The LLM container is woken by requests and can be started by hand, but the
// inactivity stop never applies to it, whoever started it.
func TestIdleStopPolicyPerService(t *testing.T) {
	wd := &fakeWd{enabled: true, resp: "ok"}
	m := newTestManager(wd)

	if err := m.control(llmService, opStart); err != nil {
		t.Fatalf("start %s: %s", llmService, err)
	}
	m.touch(llmService) // a request touching it must not arm a timer either
	st := m.states[llmService]
	st.mu.Lock()
	running, armed := st.running, st.timer != nil
	st.mu.Unlock()
	if !running {
		t.Fatalf("%s must be running after a manual start", llmService)
	}
	if armed {
		t.Fatalf("%s must never get an inactivity timer", llmService)
	}

	if err := m.control(sdService, opStart); err != nil {
		t.Fatalf("start %s: %s", sdService, err)
	}
	st = m.states[sdService]
	st.mu.Lock()
	armed = st.timer != nil
	st.mu.Unlock()
	if !armed {
		t.Fatalf("%s must still get an inactivity timer", sdService)
	}

	var llm, sd *containerState
	s := m.status()
	for i, svc := range s.Services {
		switch svc.Service {
		case llmService:
			llm = &s.Services[i]
		case sdService:
			sd = &s.Services[i]
		}
	}
	if llm == nil || sd == nil {
		t.Fatalf("status must report both %s and %s: %+v", llmService, sdService, s.Services)
	}
	if llm.IdleStop || llm.IdleStopIn != "" {
		t.Fatalf("%s must report the inactivity stop as off: %+v", llmService, llm)
	}
	if !sd.IdleStop || sd.IdleStopIn == "" {
		t.Fatalf("%s must report its remaining idle time: %+v", sdService, sd)
	}
}

// A request for the LLM starts its container when the manager believes it is
// down — the same on-demand start the image services get.
func TestLLMStartedByRequest(t *testing.T) {
	wd := &fakeWd{enabled: true, resp: "ok"}
	m := newTestManager(wd)
	if _, tracked := m.states[llmService]; !tracked {
		t.Fatalf("%s must be a tracked service", llmService)
	}
	if m.isRunning(llmService) {
		t.Fatal("no reconcile has run, so nothing may be believed running yet")
	}
	m.ensureRunning(llmService)
	if len(wd.commands) != 1 || wd.commands[0] != "start "+llmService {
		t.Fatalf("watchdog commands = %v", wd.commands)
	}
	if !m.isRunning(llmService) {
		t.Fatal("the LLM service must be marked running after the request-driven start")
	}
}

// The status page must keep answering while a start is in flight, and must say
// what is happening. A container that never comes up (an OOM restart loop, say)
// owns its service lock for the whole start budget, and one such container used
// to take the whole page down with it.
func TestStatusAnswersWhileAStartIsInFlight(t *testing.T) {
	wd := &fakeWd{enabled: true, resp: "ok", gate: make(chan struct{}), entered: make(chan string, 4)}
	m := newTestManager(wd)

	done := make(chan error, 1)
	go func() { done <- m.control(sdService, opStart) }()

	if cmd := <-wd.entered; cmd != "start "+sdService {
		t.Fatalf("watchdog command = %q", cmd)
	}

	s := statusWithin(t, m, 500*time.Millisecond)
	sd := serviceOf(s, sdService)
	if sd == nil {
		t.Fatalf("status must list %s: %+v", sdService, s.Services)
	}
	if sd.State != stateStarting {
		t.Fatalf("%s must be reported %s while its start is in flight, got %q", sdService, stateStarting, sd.State)
	}
	if len(s.Services) != len(managedServices) {
		t.Fatalf("every tracked service must still be listed, got %d", len(s.Services))
	}
	if other := serviceOf(s, comfyService); other == nil || other.State != stateStopped {
		t.Fatalf("the other services must be unaffected: %+v", other)
	}

	close(wd.gate)
	if err := <-done; err != nil {
		t.Fatalf("start: %s", err)
	}
	if after := serviceOf(m.status(), sdService); after.State != stateRunning {
		t.Fatalf("%s must be reported %s once it is up, got %q", sdService, stateRunning, after.State)
	}
}

// A container that was already up when authproxy started — the normal case when
// only authproxy is restarted — must be adopted. Left believed stopped, it never
// gets an inactivity timer, so nothing would ever stop it again.
func TestReconcileAdoptsRunningContainer(t *testing.T) {
	wd := &fakeWd{enabled: true, resp: "ok " + stateRunning}
	m := newTestManager(wd)
	m.reconcile(context.Background())

	st := m.states[comfyService]
	st.mu.Lock()
	running, armed, last := st.running, st.timer != nil, st.last
	st.mu.Unlock()
	if !running {
		t.Fatal("a container the watchdog reports running must be adopted")
	}
	if !armed {
		t.Fatal("an adopted container must get the inactivity timer, or nothing would ever stop it")
	}
	if last.IsZero() {
		t.Fatal("an adopted container starts its activity history at the adoption")
	}

	s := m.status()
	cs := serviceOf(s, comfyService)
	if cs == nil || cs.State != stateRunning || !cs.Running {
		t.Fatalf("status must report %s running: %+v", comfyService, cs)
	}
	if _, err := time.ParseDuration(cs.IdleStopIn); err != nil {
		t.Fatalf("an adopted container must report its idle countdown: %q", cs.IdleStopIn)
	}
	if _, err := time.Parse(time.RFC3339, s.ReconciledAt); err != nil {
		t.Fatalf("status must report when it was last checked: %q", s.ReconciledAt)
	}
	if s.ReconcileError != "" {
		t.Fatalf("no check failed, but the status says: %q", s.ReconcileError)
	}
	for _, cmd := range wd.commands {
		if !strings.HasPrefix(cmd, "status ") {
			t.Fatalf("reconciling must not start or stop anything, got %q", cmd)
		}
	}
}

// A reconcile must not extend the idle window of a container it already believes
// running: with a periodic check that would keep it up forever.
func TestReconcileDoesNotResetIdleWindow(t *testing.T) {
	wd := &fakeWd{enabled: true, resp: "ok"}
	m := newTestManager(wd)
	if err := m.control(comfyService, opStart); err != nil {
		t.Fatalf("start: %s", err)
	}
	st := m.states[comfyService]
	st.mu.Lock()
	before, timer := st.last, st.timer
	st.mu.Unlock()

	wd.resp = "ok " + stateRunning
	m.reconcile(context.Background())

	st.mu.Lock()
	after, same := st.last, st.timer == timer
	st.mu.Unlock()
	if !after.Equal(before) {
		t.Fatal("a running container must keep its idle window across a reconcile")
	}
	if !same {
		t.Fatal("the armed inactivity timer must not be replaced by a reconcile")
	}
}

// A container that died or was stopped by hand must not be left believed running:
// in that state the proxy keeps sending requests to a service that is not there
// and never restarts it.
func TestReconcileAdoptsStoppedContainer(t *testing.T) {
	wd := &fakeWd{enabled: true, resp: "ok"}
	m := newTestManager(wd)
	if err := m.control(comfyService, opStart); err != nil {
		t.Fatalf("start: %s", err)
	}
	wd.resp = "error: no containers found for service " + comfyService
	m.reconcile(context.Background())

	st := m.states[comfyService]
	st.mu.Lock()
	running, timer := st.running, st.timer
	st.mu.Unlock()
	if running {
		t.Fatal("a container the watchdog cannot find must not be believed running")
	}
	if timer != nil {
		t.Fatal("its inactivity timer must be stopped")
	}
	if cs := serviceOf(m.status(), comfyService); cs == nil || cs.State != stateStopped || cs.Running {
		t.Fatalf("status must report it stopped: %+v", cs)
	}
}

// A control endpoint too old to answer the status query, or simply unreachable,
// must not make the manager guess: the belief stays, and the page is told the
// view may be stale.
func TestReconcileKeepsBeliefOnFailure(t *testing.T) {
	wd := &fakeWd{enabled: true, resp: "ok"}
	m := newTestManager(wd)
	if err := m.control(comfyService, opStart); err != nil {
		t.Fatalf("start: %s", err)
	}
	wd.resp = "error: unknown op status"
	m.reconcile(context.Background())

	if !m.isRunning(comfyService) {
		t.Fatal("a failed status check must not change the belief")
	}
	s := m.status()
	if cs := serviceOf(s, comfyService); cs == nil || cs.State != stateRunning {
		t.Fatalf("the last known state must still be shown: %+v", cs)
	}
	if !strings.Contains(s.ReconcileError, "unknown op status") {
		t.Fatalf("the page must be told the check failed: %q", s.ReconcileError)
	}
}

// A service whose op is in flight is left to that op: its own answer is fresher
// than a status query issued in the middle of an operation.
func TestReconcileSkipsServiceWithOpInFlight(t *testing.T) {
	wd := &fakeWd{enabled: true, resp: "ok " + stateStopped}
	m := newTestManager(wd)
	st := m.states[comfyService]
	st.mu.Lock()
	defer st.mu.Unlock()

	m.reconcileService(context.Background(), comfyService)

	for _, cmd := range wd.commands {
		if cmd == "status "+comfyService {
			t.Fatal("no status query must be issued while an op owns the service lock")
		}
	}
}

func TestParseStatusReply(t *testing.T) {
	cases := []struct {
		resp string
		want string
		err  bool
	}{
		{"ok running", stateRunning, false},
		{"ok starting", stateStarting, false},
		{"ok unhealthy", stateUnhealthy, false},
		{"ok stopped", stateStopped, false},
		{"error: no containers found for service comfyui", stateStopped, false},
		{"error: unknown op status", "", true},
		{"ok", "", true},
		{"ok sideways", "", true},
	}
	for _, c := range cases {
		got, err := parseStatusReply(comfyService, c.resp)
		if c.err {
			if err == nil {
				t.Errorf("%q: expected an error, got %q", c.resp, got)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%q: got %q (%v), want %q", c.resp, got, err, c.want)
		}
	}
}
