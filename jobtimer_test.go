package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestJobTimerStartEnd(t *testing.T) {
	jt := NewJobTimer(2 * time.Hour)
	jt.Start("a1111", "task1", "user")
	jt.mu.Lock()
	active := jt.active
	jt.mu.Unlock()
	if active == nil || active.taskID != "task1" || active.owner != "user" || active.service != "a1111" {
		t.Fatalf("active job not set correctly: %+v", active)
	}
	jt.End("task1")
	jt.mu.Lock()
	if jt.active != nil {
		t.Fatalf("active job should be nil after End")
	}
	jt.mu.Unlock()
}

func TestJobTimerResetOnNewTask(t *testing.T) {
	jt := NewJobTimer(2 * time.Hour)
	jt.Start("a1111", "task1", "user")
	// A different taskID replaces the tracked job.
	jt.Start("a1111", "task2", "user")
	jt.mu.Lock()
	if jt.active == nil || jt.active.taskID != "task2" {
		t.Fatalf("expected active task2, got %+v", jt.active)
	}
	jt.mu.Unlock()
	// A late job_end for the previous (no longer active) task is a no-op.
	jt.End("task1")
	jt.mu.Lock()
	if jt.active == nil {
		t.Fatalf("active should still be task2 after a stale End")
	}
	jt.mu.Unlock()
}

func TestJobTimerTickAbortsOnLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	oldDefault := config.TaskTimeoutDefault
	config.TaskTimeoutDefault = "4m"
	defer func() { config.TaskTimeoutDefault = oldDefault }()

	jt := NewJobTimer(2 * time.Hour)
	jt.sdBase = srv.URL
	jt.cuiBase = srv.URL
	jt.Start("a1111", "task1", "user")
	jt.mu.Lock()
	jt.active.startTime = time.Now().Add(-10 * time.Minute) // elapsed 10m > limit 4m
	jt.mu.Unlock()

	jt.tick()
	jt.mu.Lock()
	if jt.active == nil || !jt.active.aborted {
		t.Fatalf("expected aborted=true after tick beyond limit")
	}
	jt.mu.Unlock()
}

func TestJobTimerTickNoLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	oldDefault := config.TaskTimeoutDefault
	config.TaskTimeoutDefault = ""
	defer func() { config.TaskTimeoutDefault = oldDefault }()

	jt := NewJobTimer(2 * time.Hour)
	jt.sdBase = srv.URL
	jt.cuiBase = srv.URL
	jt.Start("a1111", "task1", "user")
	jt.mu.Lock()
	jt.active.startTime = time.Now().Add(-10 * time.Minute)
	jt.mu.Unlock()

	jt.tick()
	jt.mu.Lock()
	if jt.active == nil || jt.active.aborted {
		t.Fatalf("expected no abort when no limit is configured")
	}
	jt.mu.Unlock()
}

func TestJobTimerMaxLifetime(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	jt := NewJobTimer(2 * time.Hour)
	jt.sdBase = srv.URL
	jt.cuiBase = srv.URL
	jt.Start("a1111", "task1", "user")
	jt.mu.Lock()
	// Elapsed far beyond maxLifetime (2h) -> the job is cleared, not aborted.
	jt.active.startTime = time.Now().Add(-3 * time.Hour)
	jt.mu.Unlock()

	jt.tick()
	jt.mu.Lock()
	if jt.active != nil {
		t.Fatalf("expected active cleared after max-lifetime")
	}
	jt.mu.Unlock()
}

func TestJobTimerAbortA1111(t *testing.T) {
	var gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	jt := NewJobTimer(2 * time.Hour)
	jt.sdBase = srv.URL
	jt.cuiBase = srv.URL
	jt.abort(&activeJob{service: "a1111", taskID: "abc123", owner: "user"})
	if gotPath != "/sdapi/v1/interrupt" {
		t.Fatalf("unexpected path %s", gotPath)
	}
	if gotBody != `{"task_id":"abc123"}` {
		t.Fatalf("unexpected body %q", gotBody)
	}
}

func TestJobTimerAbortComfyUI(t *testing.T) {
	var gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	jt := NewJobTimer(2 * time.Hour)
	jt.sdBase = srv.URL
	jt.cuiBase = srv.URL
	jt.abort(&activeJob{service: "comfyui", taskID: "uuid-1", owner: "user"})
	if gotPath != "/api/jobs/uuid-1/cancel" {
		t.Fatalf("unexpected path %s", gotPath)
	}
	if gotBody != "" {
		t.Fatalf("expected empty body, got %q", gotBody)
	}
}
