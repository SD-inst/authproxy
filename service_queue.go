package main

import (
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
		sq.cv.Wait()
	}
	if sq.service == t {
		return false
	}
	sq.service = t
	return true
}

func (sq *serviceQueue) setCleanup(d time.Duration) {
	sq.cancelCleanup()
	sq.cleanupTimer = time.AfterFunc(d, func() {
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
	sq.service = s
	sq.cv.Broadcast()
}
