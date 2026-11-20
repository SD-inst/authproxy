package servicequeue

import (
	"log"
	"math/rand"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

type SvcType int

const (
	NONE SvcType = iota
	SD
	LLM
	TTS
	CUI
	ACESTEP
	ACESTEP15
	WAIT = 500
)

func (s SvcType) String() string {
	switch s {
	case NONE:
		return "NONE"
	case SD:
		return "A1111"
	case LLM:
		return "LLM"
	case TTS:
		return "TTS"
	case CUI:
		return "CUI"
	case ACESTEP:
		return "ACESTEP"
	case ACESTEP15:
		return "ACESTEP1.5"
	case WAIT:
		return "WAIT"
	default:
		return "<unknown>"
	}
}

type CleanupFunc struct {
	F       func()
	Service SvcType
}

// SvcUpdate is a snapshot of the slot state taken under the queue lock, not a
// delta: it carries who holds the slot now and who held it before the last real
// change, so a consumer never has to reconstruct the history from its own
// memory of previous updates.
type SvcUpdate struct {
	Type         SvcType
	WaitType     SvcType
	PrevType     SvcType
	PrevWaitType SvcType
	Queue        int32
	Description  string
}

type ServiceQueue struct {
	sync.Mutex
	cv           *sync.Cond
	cleanupTimer *time.Timer
	cleanupID    int
	service      SvcType
	prevService  SvcType
	// prevWaitedService is the waited-for service that belonged to prevService,
	// so a WAIT state can be named in the previous slot (WAIT/LLM) instead of a
	// bare WAIT. Guarded by the queue lock, written only by SetService.
	prevWaitedService SvcType
	waitedService     SvcType
	// holdCount is the number of in-flight requests currently holding the slot
	// (acquired, or re-entered under the same holder). A request calls Hold at
	// acquisition and Release at body-close/error; the slot is only freed when
	// the last holder leaves. Caller must hold the lock.
	holdCount int
	// holdID numbers the holds in a row: it is bumped whenever the slot changes
	// state, so it identifies the hold a request joined. A holder passes the id
	// it got from Hold to Release, which lets a release of a hold that has since
	// been taken away be recognized as stale instead of freeing the current
	// holder's slot. Guarded by the queue lock.
	holdID uint64
	// holdDeadlineTimer frees the slot at the end of a bounded hold even while
	// holders are still in flight. Unlike cleanupTimer it is armed once per hold
	// and never re-armed by activity inside that hold, so a holder that keeps
	// requests arriving cannot push the bound forward. Guarded by the queue lock.
	holdDeadlineTimer *time.Timer
	CF                *CleanupFunc // executes after await if service changed
	svcChan           chan<- SvcUpdate
	waitqueue         atomic.Int32
	cleanupCV         *sync.Cond
	cleanupM          sync.Mutex
	cleanupInProgress bool
}

func NewServiceQueue(svcChan chan<- SvcUpdate) *ServiceQueue {
	result := ServiceQueue{service: NONE}
	result.cv = sync.NewCond(&result)
	result.svcChan = svcChan
	result.cleanupCV = sync.NewCond(&result.cleanupM)
	return &result
}

// Service reports the current holder of the slot. Callers must hold the queue
// lock: it is the state the condition variable guards, and read without it the
// value is only true for some earlier instant.
func (sq *ServiceQueue) Service() SvcType {
	return sq.service
}

// caller should lock and unlock sq, returns true if service has been changed or false if it was the same
func (sq *ServiceQueue) AwaitReent(t SvcType) bool {
	return sq.AwaitWithPredicateAndDescription(t, true, nil, "")
}

// allowReent finishes waiting if the service is already t, otherwise it waits for NONE
func (sq *ServiceQueue) Await(t SvcType, allowReent bool) bool {
	return sq.AwaitWithPredicateAndDescription(t, allowReent, nil, "")
}

func (sq *ServiceQueue) AwaitWithPredicateAndDescription(t SvcType, allowReent bool, p WaitPredicate, description string) bool {
	sq.AwaitCheck(t, allowReent, true, p)
	if sq.service == t { // shouldn't happen if allowReent is false
		log.Printf("*** Service is already %v, proceeding ***", t)
		sq.SetService(t, description)
		sq.CancelCleanup()
		return false
	}
	log.Printf("*** Service is %v, changing to %v ***", sq.service, t)
	sq.SetService(t, description)
	if sq.CF != nil && sq.CF.F != nil && sq.CF.Service != t {
		log.Printf("*** Running cleanup func ***")
		sq.CF.F()
		sq.CF = nil
	}
	return true
}

func (sq *ServiceQueue) maybeUpdateQueue(ql int32) <-chan bool {
	sent := make(chan bool)
	go func() {
		time.After(time.Second)
		ql2 := sq.waitqueue.Load()
		if ql == ql2 {
			sq.Lock()
			update := sq.snapshot()
			sq.Unlock()
			update.Queue = ql
			sq.svcChan <- update
			sent <- true
		} else {
			sent <- false
		}
	}()
	return sent
}

type WaitPredicate func() bool

func (sq *ServiceQueue) AwaitCheck(t SvcType, allowReent bool, queueUp bool, p WaitPredicate) {
	if queueUp {
		sentChan := sq.maybeUpdateQueue(sq.waitqueue.Add(1))
		defer func() {
			ql := sq.waitqueue.Add(-1)
			go func() {
				if sent := <-sentChan; sent {
					sq.maybeUpdateQueue(ql)
				}
			}()
		}()
	}
	for {
		if sq.service == NONE {
			break
		}
		if allowReent && sq.service == t && (p == nil || p()) {
			break
		}
		if p != nil && sq.service == WAIT && sq.waitedService == t && p() {
			break
		}
		svc := sq.service.String()
		if sq.service == WAIT {
			svc += "/" + sq.waitedService.String()
		}
		log.Printf("*** Waiting for service %v, have %v [reent: %t] p = %v ***", t, svc, allowReent, p)
		sq.cv.Wait()
	}
}

func (sq *ServiceQueue) SetCleanup(d time.Duration) {
	sq.CancelCleanup()
	sq.cleanupID = rand.Intn(100)
	log.Printf("*** Set cleanup timer id: %d, dur: %s ***", sq.cleanupID, d.String())
	sq.cleanupTimer = time.AfterFunc(d, func() {
		log.Printf("*** Running cleanup timer id: %d after %s ***", sq.cleanupID, d.String())
		sq.Lock()
		sq.SetService(NONE)
		sq.Unlock()
	})
}

func (sq *ServiceQueue) CancelCleanup() {
	if sq.cleanupTimer != nil {
		sq.cleanupTimer.Stop()
		log.Printf("*** Cancelled cleanup timer id: %d ***", sq.cleanupID)
	}
	sq.cleanupTimer = nil
}

// SetHoldDeadline bounds the current hold: at d the slot goes to NONE even if
// holders are still in flight, so whoever holds it cannot keep the GPU for more
// than d per acquisition. Unlike SetCleanup, calling it again while the same
// hold is running does not move the deadline — requests still arriving is
// exactly what must not extend the bound. The next hold (the slot changing
// hands) arms its own. d <= 0 means the hold is not bounded. Caller must hold
// the lock.
func (sq *ServiceQueue) SetHoldDeadline(d time.Duration) {
	if d <= 0 {
		sq.CancelHoldDeadline()
		return
	}
	if sq.holdDeadlineTimer != nil {
		return // this hold is already bounded; its remaining traffic cannot extend it
	}
	log.Printf("*** Set hold deadline: %s ***", d)
	sq.holdDeadlineTimer = time.AfterFunc(d, func() {
		sq.Lock()
		defer sq.Unlock()
		if sq.holdDeadlineTimer == nil {
			// The hold ended (or was re-bounded) while this callback waited for
			// the lock; there is nothing left for this deadline to do.
			return
		}
		sq.holdDeadlineTimer = nil
		if sq.service == NONE {
			return
		}
		log.Printf("*** Hold deadline %s expired: freeing the slot with %d holder(s) still in flight ***", d, sq.holdCount)
		sq.SetService(NONE)
	})
}

// CancelHoldDeadline disarms the bounded hold of the current holder. Caller must
// hold the lock.
func (sq *ServiceQueue) CancelHoldDeadline() {
	if sq.holdDeadlineTimer != nil {
		sq.holdDeadlineTimer.Stop()
		log.Printf("*** Cancelled hold deadline ***")
		sq.holdDeadlineTimer = nil
	}
}

// Hold records that one more request is now holding the slot and returns the id
// of the hold it joined. It must be paired with a later Release carrying that id
// (one per request: Hold at acquisition, Release at body-close or proxy error).
// Caller must hold the lock.
func (sq *ServiceQueue) Hold() uint64 {
	sq.holdCount++
	return sq.holdID
}

// Release marks one holder as done. hold is the id the holder got from Hold: if
// the slot changed state since then, the holder's slot was already taken away
// (a hold deadline or a hard-cap timer fired while its request was in flight)
// and its release belongs to a hold that no longer exists — acting on the count
// of whoever holds the slot now would free it out from under them. When it is
// the last holder of the current hold, the slot is freed (WAIT+grace if
// afterBody > 0, else NONE); otherwise the slot stays held for the remaining
// holders and the hard cap is re-armed so they stay bounded. Caller must hold
// the lock.
func (sq *ServiceQueue) Release(hold uint64, hardCap, afterBody time.Duration) {
	if hold != sq.holdID {
		log.Printf("*** Release: ignoring release of hold %d, the slot is on hold %d ***", hold, sq.holdID)
		return
	}
	if sq.holdCount > 0 {
		sq.holdCount--
	}
	if sq.holdCount > 0 {
		log.Printf("*** Release: %d holder(s) remaining, keeping slot ***", sq.holdCount)
		sq.SetCleanup(hardCap)
		return
	}
	log.Printf("*** Release: last holder, freeing slot ***")
	if sq.service == NONE {
		// Nothing to hand over: the slot is already free. Arming the after-body
		// grace on a free slot would invent a WAIT state for a holder that no
		// longer exists, and the status page would show a transition that never
		// happened.
		log.Printf("*** Release: slot is already free, nothing to release ***")
		return
	}
	sq.CancelCleanup()
	if afterBody > 0 {
		sq.SetService(WAIT)
		sq.SetCleanup(afterBody)
	} else {
		sq.SetService(NONE)
	}
}

// snapshot copies the guarded slot state into an update. Callers must hold the
// queue lock.
func (sq *ServiceQueue) snapshot() SvcUpdate {
	return SvcUpdate{
		Type:         sq.service,
		WaitType:     sq.waitedService,
		PrevType:     sq.prevService,
		PrevWaitType: sq.prevWaitedService,
		Queue:        sq.waitqueue.Load(),
	}
}

// SetService moves the slot to s and publishes the resulting state. Re-entering
// the service that already holds the slot changes nothing but the description:
// the recorded previous holder stays the one from the last real transition, so
// the status page keeps showing where the current task came from instead of
// "X ⇐ X" for every progress beat. Must be called under the queue lock.
func (sq *ServiceQueue) SetService(s SvcType, description ...string) {
	desc := ""
	if len(description) > 0 {
		desc = description[0]
	}
	if sq.service != s {
		log.Printf("*** Setting service to %v ***", s)
		// A real state change ends the hold that was in progress: whoever still
		// has a request of that hold running must not be able to free what is no
		// longer theirs, and a bounded hold belongs to the holder it was armed
		// for. Re-entering the same service is not a change and keeps both.
		sq.holdID++
		sq.CancelHoldDeadline()
		sq.prevService = sq.service
		sq.prevWaitedService = sq.waitedService
		switch s {
		case WAIT:
			if sq.service != WAIT && sq.service != NONE {
				sq.waitedService = sq.service
				sq.service = s
				log.Printf("*** Setting service waiting to %v ***", sq.waitedService)
			} else {
				sq.service = NONE
				sq.waitedService = NONE
			}
		case NONE:
			sq.waitedService = NONE
			sq.service = NONE
			// Reaching NONE means no holder owns the slot. Reset the count so a
			// holder that leaked (freed by the hard cap without a Release) doesn't
			// leave a stale count that would block the next acquisition.
			sq.holdCount = 0
		default:
			sq.service = s
		}
	}
	sq.cv.Broadcast()
	update := sq.snapshot()
	update.Description = desc
	sq.svcChan <- update
}

// UpdateProgressDescription re-publishes the current slot state with a fresh
// task line ("rendering 12/20 steps") without changing who holds the slot. It
// takes the queue lock itself: its caller is the status-page poller, which does
// not own the state machine. Returns false when there is no task to describe.
func (sq *ServiceQueue) UpdateProgressDescription(description string) bool {
	sq.Lock()
	defer sq.Unlock()
	if sq.service == NONE {
		return false
	}
	update := sq.snapshot()
	update.Description = description
	sq.svcChan <- update
	return true
}

func (sq *ServiceQueue) ServiceCloser(t SvcType, pathChecker func(path string) bool, timeout time.Duration, closeOnBody bool) func(req *http.Request, resp *http.Response) error {
	return sq.ServiceCloserWithAfterBody(t, pathChecker, timeout, closeOnBody, nil)
}

func (sq *ServiceQueue) ServiceCloserWithAfterBody(t SvcType, pathChecker func(path string) bool, timeout time.Duration, closeOnBody bool, waitAfterBody func(req *http.Request) time.Duration) func(req *http.Request, resp *http.Response) error {
	return func(req *http.Request, resp *http.Response) error {
		path := req.URL.Path
		if !pathChecker(path) {
			return nil
		}
		log.Printf("*** Setting closer for %s, %s ***", path, timeout)
		sq.Lock()
		sq.AwaitReent(t)
		log.Printf("*** Closer wait for %s is over ***", path)
		// The hold this response belongs to, as of now: AwaitReent above may have
		// taken the slot again for it, and the release on body-close must act on
		// that hold and not on one that has already been handed over.
		hold := sq.holdID
		if closeOnBody {
			if resp != nil {
				resp.Body = BodyWrapper{ReadCloser: resp.Body, onClose: func() {
					log.Printf("*** Closing body ***")
					sq.Lock()
					var afterBody time.Duration
					if waitAfterBody != nil {
						afterBody = waitAfterBody(req)
					}
					sq.Release(hold, timeout, afterBody)
					sq.Unlock()
				}}
			} else {
				log.Printf("*** No response set ***")
				sq.Release(hold, timeout, 0)
				sq.Unlock()
				return nil
			}
		}
		sq.SetCleanup(timeout)
		sq.Unlock()
		return nil
	}
}

// ServiceCloserWithRelease is the response-time half of an acquisition the
// caller has already made: it normalizes the slot state when the upstream
// responds and installs the caller's release func on the response body. The
// release is supplied by the caller instead of being an internal Release, so
// that the caller can make it idempotent — the body-close hook and a proxy
// error handler can both fire for one request, and each holder must free the
// slot at most once. resp must be non-nil (a response was received).
func (sq *ServiceQueue) ServiceCloserWithRelease(t SvcType, timeout time.Duration, release func()) func(req *http.Request, resp *http.Response) error {
	return func(req *http.Request, resp *http.Response) error {
		sq.Lock()
		sq.AwaitReent(t)
		log.Printf("*** Closer wait for %s is over ***", req.URL.Path)
		resp.Body = BodyWrapper{ReadCloser: resp.Body, onClose: func() {
			log.Printf("*** Closing body ***")
			release()
		}}
		sq.SetCleanup(timeout)
		sq.Unlock()
		return nil
	}
}

func (sq *ServiceQueue) SetCleanupProgress(done bool) {
	sq.cleanupM.Lock()
	defer sq.cleanupM.Unlock()
	sq.cleanupInProgress = done
	if done {
		sq.cleanupCV.Broadcast()
	}
}

func (sq *ServiceQueue) WaitForCleanup(timeout time.Duration) (timedout bool) {
	sq.cleanupM.Lock()
	defer sq.cleanupM.Unlock()
	time.AfterFunc(timeout, func() {
		timedout = true
		sq.cleanupCV.Broadcast()
	})
	for !sq.cleanupInProgress {
		sq.cleanupCV.Wait()
		if timedout {
			return
		}
	}
	return
}
