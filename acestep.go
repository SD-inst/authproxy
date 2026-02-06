package main

import (
	"net/http"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/rkfg/authproxy/servicequeue"
)

func addASQueueHandlers(e *echo.Echo, sq *servicequeue.ServiceQueue) {
	e.POST("/acestep/join", func(c echo.Context) error {
		sq.Lock()
		sq.AwaitReent(servicequeue.ACESTEP)
		sq.Unlock()
		sq.SetCleanup(time.Minute)
		return nil
	})
	e.POST("/acestep/leave", func(c echo.Context) error {
		sq.Lock()
		sq.AwaitReent(servicequeue.ACESTEP)
		sq.SetCleanup(time.Second * 3)
		sq.Unlock()
		return nil
	})
	cleanupClient := http.Client{Timeout: time.Second * 10}
	e.POST("/acestep15/join", func(c echo.Context) error {
		sq.Lock()
		sq.AwaitReent(servicequeue.ACESTEP15)
		sq.CF = &servicequeue.CleanupFunc{
			F: func() {
				cleanupClient.Post("http://acestep15:7860/unload_llm", echo.MIMEApplicationJSON, nil)
			},
			Service: servicequeue.ACESTEP15,
		}
		sq.Unlock()
		sq.SetCleanup(time.Minute)
		return nil
	})
	e.POST("/acestep15/leave", func(c echo.Context) error {
		sq.Lock()
		sq.AwaitReent(servicequeue.ACESTEP15)
		sq.CancelCleanup()
		sq.SetService(servicequeue.NONE)
		sq.Unlock()
		return nil
	})
}
