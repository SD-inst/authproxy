package main

import (
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
}
