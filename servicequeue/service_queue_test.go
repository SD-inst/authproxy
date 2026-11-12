package servicequeue

import (
	"testing"
	"time"
)

// newTestQueue builds a queue whose published updates can be read back, and
// returns the queue together with the channel the updates land on.
func newTestQueue() (*ServiceQueue, chan SvcUpdate) {
	ch := make(chan SvcUpdate, 10)
	return NewServiceQueue(ch), ch
}

// lastUpdate drains the published updates, failing the test when none was
// published at all.
func lastUpdate(t *testing.T, ch chan SvcUpdate) SvcUpdate {
	t.Helper()
	var last SvcUpdate
	found := false
	for {
		select {
		case last = <-ch:
			found = true
		default:
			if !found {
				t.Fatal("no service update was published")
			}
			return last
		}
	}
}

// Every update is a snapshot of the slot, so the status page does not have to
// remember earlier updates to name where the current task came from.
func TestSetServicePublishesPreviousHolder(t *testing.T) {
	sq, ch := newTestQueue()

	sq.Lock()
	sq.SetService(LLM, "model-x")
	sq.Unlock()
	if u := lastUpdate(t, ch); u.Type != LLM || u.PrevType != NONE {
		t.Errorf("expected LLM acquired from NONE, got %v ⇐ %v", u.Type, u.PrevType)
	}

	sq.Lock()
	sq.SetService(CUI, "preparing...")
	sq.Unlock()
	if u := lastUpdate(t, ch); u.Type != CUI || u.PrevType != LLM {
		t.Errorf("expected CUI acquired from LLM, got %v ⇐ %v", u.Type, u.PrevType)
	}
}

// A service re-entering the slot it already holds (every ComfyUI progress beat,
// every chained LLM request) must not rewrite the previous holder to itself,
// or the page shows "CUI ⇐ CUI" and loses the transition the user cares about.
func TestSetServiceReentryKeepsPreviousHolder(t *testing.T) {
	sq, ch := newTestQueue()

	sq.Lock()
	sq.SetService(LLM)
	sq.SetService(LLM, "model-x")
	sq.Unlock()

	u := lastUpdate(t, ch)
	if u.Type != LLM || u.PrevType != NONE {
		t.Errorf("expected the re-entry to keep NONE as the previous holder, got %v ⇐ %v", u.Type, u.PrevType)
	}
	if u.Description != "model-x" {
		t.Errorf("expected the re-entry to still publish its description, got %q", u.Description)
	}
}

// A WAIT state has to name the service it is waiting on both for itself and for
// the transition out of it, so the page renders "WAIT/LLM" rather than a bare
// WAIT on either side.
func TestWaitStateNamesTheWaitedService(t *testing.T) {
	sq, ch := newTestQueue()

	sq.Lock()
	sq.SetService(LLM)
	sq.SetService(WAIT)
	sq.Unlock()
	if u := lastUpdate(t, ch); u.Type != WAIT || u.WaitType != LLM || u.PrevType != LLM {
		t.Errorf("expected WAIT/LLM from LLM, got %v/%v ⇐ %v", u.Type, u.WaitType, u.PrevType)
	}

	sq.Lock()
	sq.SetService(NONE)
	sq.Unlock()
	u := lastUpdate(t, ch)
	if u.Type != NONE || u.PrevType != WAIT || u.PrevWaitType != LLM {
		t.Errorf("expected NONE ⇐ WAIT/LLM, got %v ⇐ %v/%v", u.Type, u.PrevType, u.PrevWaitType)
	}
}

// A holder whose slot was already freed by its hard-cap timer must not arm the
// after-body grace on top of a free slot: that invents a WAIT state for a
// holder that no longer exists.
func TestReleaseOnFreedSlotKeepsItFree(t *testing.T) {
	sq, ch := newTestQueue()

	sq.Lock()
	sq.SetService(LLM)
	sq.Hold()
	// What the hard-cap cleanup timer does while the request is still running.
	sq.SetService(NONE)
	sq.Release(0, 10*time.Second)
	sq.Unlock()

	if u := lastUpdate(t, ch); u.Type != NONE {
		t.Errorf("expected the freed slot to stay NONE after a late Release, got %v/%v", u.Type, u.WaitType)
	}
	if sq.holdCount != 0 {
		t.Errorf("expected the holder count to be cleared, got %d", sq.holdCount)
	}
}
