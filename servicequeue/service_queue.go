package servicequeue

import (
	"log"
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
	OVI
	ACESTEP15
	WAIT   = 500
	IGNORE = 999
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
	case OVI:
		return "OVI"
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

type SvcUpdate struct {
	Type     SvcType
	WaitType SvcType
	Queue    int32
}

type ServiceQueue struct {
	sync.Mutex
	cv                *sync.Cond
	cleanupTimer      *time.Timer
	service           SvcType
	waitedService     SvcType
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

// caller should lock and unlock sq, returns true if service has been changed or false if it was the same
func (sq *ServiceQueue) AwaitReent(t SvcType) bool {
	return sq.AwaitWithPredicate(t, true, nil)
}

// allowReent finishes waiting if the service is already t, otherwise it waits for NONE
func (sq *ServiceQueue) Await(t SvcType, allowReent bool) bool {
	return sq.AwaitWithPredicate(t, allowReent, nil)
}

func (sq *ServiceQueue) AwaitWithPredicate(t SvcType, allowReent bool, p WaitPredicate) bool {
	sq.AwaitCheck(t, allowReent, true, p)
	if sq.service == t { // shouldn't happen if allowReent is false
		log.Printf("*** Service is already %v, proceeding ***", t)
		sq.CancelCleanup()
		return false
	}
	log.Printf("*** Service is %v, changing to %v ***", sq.service, t)
	sq.SetService(t)
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
			sq.svcChan <- SvcUpdate{Type: IGNORE, Queue: ql}
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
		if allowReent && sq.service == t {
			break
		}
		if p != nil && sq.service == WAIT && sq.waitedService == t && p() {
			break
		}
		log.Printf("*** Waiting for service %v, have %v [reent: %t] ***", t, sq.service, allowReent)
		sq.cv.Wait()
	}
}

func (sq *ServiceQueue) SetCleanup(d time.Duration) {
	sq.CancelCleanup()
	sq.cleanupTimer = time.AfterFunc(d, func() {
		log.Print("*** Cleanup timer ***")
		sq.SetService(NONE)
	})
}

func (sq *ServiceQueue) CancelCleanup() {
	if sq.cleanupTimer != nil {
		sq.cleanupTimer.Stop()
	}
	sq.cleanupTimer = nil
}

// should be called under lock
func (sq *ServiceQueue) SetService(s SvcType) {
	log.Printf("*** Setting service to %v ***", s)
	if s == WAIT {
		sq.waitedService = sq.service
		log.Printf("*** Setting service waiting to %v ***", sq.waitedService)
	}
	sq.service = s
	sq.cv.Broadcast()
	sq.svcChan <- SvcUpdate{Type: s, WaitType: sq.waitedService, Queue: sq.waitqueue.Load()}
}

func (sq *ServiceQueue) ServiceCloser(t SvcType, pathChecker func(path string) bool, timeout time.Duration, closeOnBody bool) func(req *http.Request, resp *http.Response) error {
	return sq.ServiceCloserWithAfterBody(t, pathChecker, timeout, closeOnBody, 0)
}

func (sq *ServiceQueue) ServiceCloserWithAfterBody(t SvcType, pathChecker func(path string) bool, timeout time.Duration, closeOnBody bool, waitAfterBody time.Duration) func(req *http.Request, resp *http.Response) error {
	return func(req *http.Request, resp *http.Response) error {
		path := req.URL.Path
		if !pathChecker(path) {
			return nil
		}
		log.Printf("*** Setting closer for %s, %s ***", path, timeout)
		sq.Lock()
		sq.AwaitReent(t)
		log.Printf("*** Closer wait for %s is over ***", path)
		if closeOnBody {
			if resp != nil {
				resp.Body = BodyWrapper{ReadCloser: resp.Body, onClose: func() {
					log.Printf("*** Closing body ***")
					sq.Lock()
					sq.CancelCleanup()
					if waitAfterBody > 0 {
						sq.SetService(WAIT)
						sq.SetCleanup(waitAfterBody)
					} else {
						sq.SetService(NONE)
					}
					sq.Unlock()
				}}
			} else {
				log.Printf("*** No response set ***")
				sq.CancelCleanup()
				sq.SetService(NONE)
				sq.Unlock()
				return nil
			}
		}
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
