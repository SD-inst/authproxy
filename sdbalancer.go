package main

import (
	"time"

	"github.com/labstack/echo/v4"
	"github.com/rkfg/authproxy/progress"
	"github.com/rkfg/authproxy/servicequeue"
)

func addSDQueueHandlers(e *echo.Echo, sq *servicequeue.ServiceQueue, pr progress.TaskColorSetter) {
	cleanupFunc := &servicequeue.CleanupFunc{
		F: func() {
			post("/sdapi/v1/unload-checkpoint")
		},
		Service: servicequeue.SD,
	}
	sq.CF = cleanupFunc // SD is supposed to be active by default
	e.POST("/internal/join", func(c echo.Context) error {
		user := authUser(c) // the Gradio page sends the sdkey cookie
		sq.Lock()
		defer sq.Unlock()
		if sq.AwaitReent(servicequeue.SD) {
			post("/sdapi/v1/reload-checkpoint")
		}
		sq.CF = cleanupFunc
		sq.SetService(servicequeue.SD, "preparing...")
		pr.SetTaskColor(taskColor(user))
		return nil
	})
	e.POST("/internal/leave", func(c echo.Context) error {
		sq.Lock()
		defer sq.Unlock()
		sq.AwaitReent(servicequeue.SD)
		sq.SetCleanup(time.Second * 7)
		return nil
	})
}
