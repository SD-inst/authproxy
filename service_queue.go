package main

import (
	"log"
	"net/http"
	"sync"
	"time"
)

type svcType int

const (
	NONE svcType = iota
	SD
	LLM
	TTS
	CUI
)

type cleanupFunc struct {
	f       func()
	service svcType
}

type serviceQueue struct {
	sync.Mutex
	cv           *sync.Cond
	cleanupTimer *time.Timer
	service      svcType
	cf           *cleanupFunc // executes after await if service changed
}

func newServiceQueue() *serviceQueue {
	result := serviceQueue{service: NONE}
	result.cv = sync.NewCond(&result)
	return &result
}

// caller should lock and unlock sq, returns true if service has been changed or false if it was the same
func (sq *serviceQueue) await(t svcType) bool {
	for sq.service != t && sq.service != NONE {
		log.Printf("*** Waiting for service %v, have %v ***", t, sq.service)
		sq.cv.Wait()
	}
	if sq.service == t {
		log.Printf("*** Service is already %v, proceeding ***", t)
		sq.cancelCleanup()
		return false
	}
	log.Printf("*** Service is %v, changing ***", sq.service)
	sq.setService(t)
	if sq.cf != nil && sq.cf.f != nil && sq.cf.service != t {
		log.Printf("*** Running cleanup func ***")
		sq.cf.f()
		sq.cf = nil
	}
	return true
}

func (sq *serviceQueue) setCleanup(d time.Duration) {
	sq.cancelCleanup()
	sq.cleanupTimer = time.AfterFunc(d, func() {
		log.Print("*** Cleanup timer ***")
		sq.setService(NONE)
	})
}

func (sq *serviceQueue) cancelCleanup() {
	if sq.cleanupTimer != nil {
		sq.cleanupTimer.Stop()
	}
	sq.cleanupTimer = nil
}

// should be called under lock
func (sq *serviceQueue) setService(s svcType) {
	log.Printf("*** Setting service to %v ***", s)
	sq.service = s
	sq.cv.Broadcast()
}

func (sq *serviceQueue) serviceCloser(t svcType, pathChecker func(path string) bool, timeout time.Duration, closeOnBody bool) func(req *http.Request, resp *http.Response) error {
	return func(req *http.Request, resp *http.Response) error {
		path := req.URL.Path
		if !pathChecker(path) {
			return nil
		}
		log.Printf("*** Setting closer for %s, %s ***", path, timeout)
		sq.Lock()
		sq.await(t)
		log.Printf("*** Closer wait for %s is over ***", path)
		if closeOnBody {
			if resp != nil {
				resp.Body = bodyWrapper{ReadCloser: resp.Body, onClose: func() {
					log.Printf("*** Closing body ***")
					sq.Lock()
					sq.cancelCleanup()
					sq.setService(NONE)
					sq.Unlock()
				}}
			} else {
				log.Printf("*** No response set ***")
			}
		}
		sq.setCleanup(timeout)
		sq.Unlock()
		return nil
	}
}
