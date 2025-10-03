package main

import (
	"time"

	"github.com/labstack/echo/v4"
	"github.com/rkfg/authproxy/servicequeue"
)

func addOviQueueHandlers(e *echo.Echo, sq *servicequeue.ServiceQueue) {
	e.POST("/ovi/join", func(c echo.Context) error {
		sq.Lock()
		sq.AwaitReent(servicequeue.OVI)
		sq.Unlock()
		sq.SetCleanup(time.Minute * 5)
		return nil
	})
	e.POST("/ovi/leave", func(c echo.Context) error {
		sq.Lock()
		sq.AwaitReent(servicequeue.OVI)
		sq.SetCleanup(time.Second * 3)
		sq.Unlock()
		return nil
	})
}
