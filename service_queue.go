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
)

type serviceQueue struct {
	sync.Mutex
	cv           *sync.Cond
	cleanupTimer *time.Timer
	service      svcType
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
		return false
	}
	log.Printf("*** Service is %v, changing ***", sq.service)
	sq.setService(t)
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

func (sq *serviceQueue) serviceCloser(pathChecker func(path string) bool, timeout time.Duration, closeOnBody bool) func(r *http.Response) error {
	return func(r *http.Response) error {
		path := r.Request.URL.Path
		if pathChecker(path) {
			if closeOnBody {
				r.Body = bodyWrapper{ReadCloser: r.Body, onClose: func() {
					log.Printf("*** Closing body ***")
					sq.Lock()
					sq.cancelCleanup()
					sq.setService(NONE)
					sq.Unlock()
				}}
			}
			sq.Lock()
			sq.setCleanup(timeout)
			sq.Unlock()
		}
		return nil
	}
}
