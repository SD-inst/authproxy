package main

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"
)

// activeJob is the single tracked job. There is one GPU slot (servicequeue), so
// at most one job runs at a time; the timer keeps exactly one active job.
type activeJob struct {
	service   string
	taskID    string
	owner     string
	startTime time.Time
	aborted   bool
	limit     time.Duration
	limitOK   bool
}

// JobTimer enforces the per-user task time limit. A service reports job_start
// (service, task_id, owner) when a task begins and job_end (task_id) when it
// finishes. The timer runs a 1s ticker: once the wall-clock elapsed time since
// job_start exceeds the owner's limit, it fires the service's interrupt (carrying
// the task_id so a stale abort is a no-op). A max-lifetime safety clears a job
// that never ended (lost job_end / container restart).
type JobTimer struct {
	mu          sync.Mutex
	active      *activeJob
	maxLifetime time.Duration
	sdBase      string
	cuiBase     string
	client      *http.Client
}

// NewJobTimer builds a timer that clears a job after maxLifetime.
func NewJobTimer(maxLifetime time.Duration) *JobTimer {
	return &JobTimer{
		maxLifetime: maxLifetime,
		sdBase:      SD_URL,
		cuiBase:     CUI_URL,
		client:      &http.Client{Timeout: 5 * time.Second},
	}
}

// Start begins tracking a job. If the taskID differs from the currently
// tracked one, the job is replaced (a new task overwrites the previous).
// Re-starting the same taskID is a no-op (does not reset the clock).
func (jt *JobTimer) Start(service, taskID, owner string) {
	jt.mu.Lock()
	defer jt.mu.Unlock()
	if jt.active != nil && jt.active.taskID == taskID {
		return
	}
	limit, limitOK := taskLimit(owner)
	jt.active = &activeJob{service: service, taskID: taskID, owner: owner, startTime: time.Now(), aborted: false, limit: limit, limitOK: limitOK}
	log.Printf("Job started: service=%s task=%s owner=%s", service, taskID, owner)
}

// End stops tracking a job. A job_end for a task that is no longer the active
// one (e.g. a late end after a new job started) is a no-op.
func (jt *JobTimer) End(taskID string) {
	jt.mu.Lock()
	defer jt.mu.Unlock()
	if jt.active != nil && jt.active.taskID == taskID {
		log.Printf("Job ended: task=%s", taskID)
		jt.active = nil
	}
}

// Run begins the 1s ticker that checks the active job.
func (jt *JobTimer) Run() {
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for range t.C {
			jt.tick()
		}
	}()
}

// tick checks the active job once per second and fires the abort when the limit
// is exceeded, or clears the job once it has outlived the max-lifetime safety.
func (jt *JobTimer) tick() {
	jt.mu.Lock()
	defer jt.mu.Unlock()
	if jt.active == nil || jt.active.aborted {
		return
	}
	elapsed := time.Since(jt.active.startTime)
	if elapsed > jt.maxLifetime {
		log.Printf("Job %s (owner %s) exceeded max lifetime %s, clearing (lost job_end?)", jt.active.taskID, jt.active.owner, jt.maxLifetime)
		jt.active = nil
		return
	}
	if jt.active.limitOK && elapsed > jt.active.limit {
		jt.active.aborted = true
		log.Printf("Job %s (owner %s) exceeded limit %s, aborting", jt.active.taskID, jt.active.owner, jt.active.limit)
		go jt.abort(jt.active)
	}
}

// abort issues the service's interrupt, carrying the task_id so the service only
// aborts a job that is still the current one (stale abort = no-op). A1111:
// POST /sdapi/v1/interrupt {"task_id": ...}; ComfyUI: POST /api/jobs/<id>/cancel.
func (jt *JobTimer) abort(j *activeJob) {
	var url, body string
	switch j.service {
	case "a1111":
		url = jt.sdBase + "/sdapi/v1/interrupt"
		body = fmt.Sprintf(`{"task_id":%q}`, j.taskID)
	case "comfyui":
		url = jt.cuiBase + "/api/jobs/" + j.taskID + "/cancel"
	default:
		log.Printf("Unknown service %q for job %s, cannot abort", j.service, j.taskID)
		return
	}
	contentType := "application/json"
	var rdr io.Reader
	if body != "" {
		rdr = bytes.NewBufferString(body)
	}
	resp, err := jt.client.Post(url, contentType, rdr)
	if err != nil {
		log.Printf("Error aborting job %s (%s): %s", j.taskID, url, err)
		return
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	log.Printf("Job %s abort sent to %s (HTTP %d)", j.taskID, url, resp.StatusCode)
}
