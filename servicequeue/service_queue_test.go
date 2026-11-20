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
	hold := sq.Hold()
	// What the hard-cap cleanup timer does while the request is still running.
	sq.SetService(NONE)
	sq.Release(hold, 0, 10*time.Second)
	sq.Unlock()

	if u := lastUpdate(t, ch); u.Type != NONE {
		t.Errorf("expected the freed slot to stay NONE after a late Release, got %v/%v", u.Type, u.WaitType)
	}
	if sq.holdCount != 0 {
		t.Errorf("expected the holder count to be cleared, got %d", sq.holdCount)
	}
}

// The hold deadline is what bounds a holder that never stops: it frees the slot
// at d even though holders are still in flight, exactly as the hard-cap timer
// does, so whoever is queued behind gets the GPU.
func TestHoldDeadlineFreesSlotWithHoldersInFlight(t *testing.T) {
	sq, ch := newTestQueue()

	sq.Lock()
	sq.SetService(LLM)
	sq.Hold()
	sq.Hold() // a second parallel thread of the same holder
	sq.SetHoldDeadline(50 * time.Millisecond)
	sq.Unlock()

	if u := waitForUpdate(t, ch, NONE, 2*time.Second); u.Type != NONE {
		t.Fatalf("expected the deadline to free the slot, the queue last said %v", u.Type)
	}
	sq.Lock()
	defer sq.Unlock()
	if sq.service != NONE {
		t.Errorf("the slot must be NONE while both holders are still in flight, got %v", sq.service)
	}
}

// Traffic inside a bounded hold must not move its deadline: that is the whole
// point of the bound, since several parallel threads of one holder keep the
// slot by arriving again and again. Re-arming during the same hold is a no-op,
// so the short deadline set first is what fires.
func TestHoldDeadlineIsNotExtendedByTraffic(t *testing.T) {
	sq, ch := newTestQueue()

	sq.Lock()
	sq.SetService(LLM)
	sq.Hold()
	sq.SetHoldDeadline(50 * time.Millisecond)
	// What the holder's next requests would do if the bound followed activity.
	sq.SetHoldDeadline(time.Hour)
	sq.SetHoldDeadline(time.Hour)
	sq.Unlock()

	if u := waitForUpdate(t, ch, NONE, 2*time.Second); u.Type != NONE {
		t.Fatalf("re-arming the deadline extended the hold, the queue last said %v", u.Type)
	}
}

// The deadline belongs to the hold it was armed for. After a handover it must
// not fire on the new holder: the slot stays with whoever took it over.
func TestHoldDeadlineDoesNotOutliveItsHold(t *testing.T) {
	sq, ch := newTestQueue()

	sq.Lock()
	sq.SetService(LLM)
	sq.Hold()
	sq.SetHoldDeadline(30 * time.Millisecond)
	sq.SetService(SD) // the LLM finished normally and A1111 took the slot
	sq.Unlock()

	// Give the armed deadline more than enough time to (wrongly) fire.
	time.Sleep(120 * time.Millisecond)
	if u := lastUpdate(t, ch); u.Type != SD {
		t.Errorf("the deadline of the previous hold must not free the slot of the next one, got %v", u.Type)
	}
}

// A holder whose slot was taken away mid-request (a hold deadline or a hard cap
// fired) must not free the slot of whoever holds it now when its own request
// finally closes: that would hand the GPU to a third service while the current
// holder is still running, which is what the queue exists to prevent.
func TestStaleReleaseKeepsTheCurrentHolder(t *testing.T) {
	sq, ch := newTestQueue()

	sq.Lock()
	sq.SetService(LLM)
	stale := sq.Hold()
	sq.SetService(NONE) // what the hold deadline does while the request runs
	sq.SetService(LLM)  // and a new holder takes the slot over
	current := sq.Hold()
	sq.Release(stale, time.Minute, 0)
	if sq.service != LLM {
		t.Fatalf("a stale release freed the slot of the current holder: %v", sq.service)
	}
	if sq.holdCount != 1 {
		t.Errorf("a stale release must not touch the current holder's count, got %d", sq.holdCount)
	}
	sq.Release(current, time.Minute, 0)
	sq.Unlock()

	if u := lastUpdate(t, ch); u.Type != NONE {
		t.Errorf("the release of the current holder must still free the slot, got %v", u.Type)
	}
}

// waitForUpdate drains the published updates until one reports state want, or
// the timeout runs out; either way the last update seen is returned.
func waitForUpdate(t *testing.T, ch chan SvcUpdate, want SvcType, timeout time.Duration) SvcUpdate {
	t.Helper()
	deadline := time.After(timeout)
	var last SvcUpdate
	for {
		select {
		case u := <-ch:
			last = u
			if u.Type == want {
				return last
			}
		case <-deadline:
			return last
		}
	}
}
