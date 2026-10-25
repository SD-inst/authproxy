package main

import (
	"context"
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
}

func (f *fakeWd) Enabled() bool { return f.enabled }

func (f *fakeWd) Exec(_ context.Context, command string) (string, error) {
	f.commands = append(f.commands, command)
	return f.resp, f.err
}

func newTestManager(wd wdClient) *containerManager {
	return newContainerManager(wd, 60*time.Minute)
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
		t.Fatal("nothing is running at startup")
	}
	m.ensureRunning(llmService)
	if len(wd.commands) != 1 || wd.commands[0] != "start "+llmService {
		t.Fatalf("watchdog commands = %v", wd.commands)
	}
	if !m.isRunning(llmService) {
		t.Fatal("the LLM service must be marked running after the request-driven start")
	}
}
