package events

import (
	"github.com/gorilla/websocket"
	"github.com/labstack/echo/v4"
)

func (b *Broker) WSHandler(c echo.Context) error {
	upg := websocket.Upgrader{}
	conn, err := upg.Upgrade(c.Response(), c.Request(), nil)
	if err != nil {
		return err
	}
	defer conn.Close()
	ch := make(chan Packet, 100)
	s := subscriber{ch: ch, ip: c.RealIP()}
	b.addSub <- s
	defer func() { b.delSub <- s }()
	quit := make(chan struct{})
	b.reqInit <- requestInit{ch: ch, stateType: PROGRESS_UPDATE}
	b.reqInit <- requestInit{ch: ch, stateType: GPU_UPDATE}
	b.reqInit <- requestInit{ch: ch, stateType: SERVICE_UPDATE}
	go func() {
		var v string
		for {
			err := conn.ReadJSON(&v)
			if err != nil {
				close(quit)
				return
			}
		}
	}()
	for {
		select {
		case p := <-ch:
			conn.WriteJSON(p)
		case <-quit:
			return nil
		}
	}
}
