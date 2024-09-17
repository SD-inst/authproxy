package servicequeue

import (
	"log"
	"net/http"
	"sync"
	"time"
)

type SvcType int

const (
	NONE SvcType = iota
	SD
	LLM
	TTS
	CUI
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
	default:
		return "<unknown>"
	}
}

type CleanupFunc struct {
	F       func()
	Service SvcType
}

type ServiceQueue struct {
	sync.Mutex
	cv           *sync.Cond
	cleanupTimer *time.Timer
	service      SvcType
	CF           *CleanupFunc // executes after await if service changed
	svcChan      chan<- SvcType
}

func NewServiceQueue(svcChan chan<- SvcType) *ServiceQueue {
	result := ServiceQueue{service: NONE}
	result.cv = sync.NewCond(&result)
	result.svcChan = svcChan
	return &result
}

// caller should lock and unlock sq, returns true if service has been changed or false if it was the same
func (sq *ServiceQueue) AwaitReent(t SvcType) bool {
	return sq.Await(t, true)
}

// allowReent finishes waiting if the service is already t, otherwise it waits for NONE
func (sq *ServiceQueue) Await(t SvcType, allowReent bool) bool {
	sq.AwaitCheck(t, allowReent)
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

func (sq *ServiceQueue) AwaitCheck(t SvcType, allowReent bool) {
	for (sq.service != t || !allowReent) && sq.service != NONE {
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
	sq.service = s
	sq.cv.Broadcast()
	sq.svcChan <- s
}

func (sq *ServiceQueue) ServiceCloser(t SvcType, pathChecker func(path string) bool, timeout time.Duration, closeOnBody bool) func(req *http.Request, resp *http.Response) error {
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
					sq.SetService(NONE)
					sq.Unlock()
				}}
			} else {
				log.Printf("*** No response set ***")
			}
		}
		sq.SetCleanup(timeout)
		sq.Unlock()
		return nil
	}
}
