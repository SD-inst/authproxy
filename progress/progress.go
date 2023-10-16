package progress

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
	"github.com/labstack/echo/v4"
)

//go:embed webroot
var webroot embed.FS

type packetType string

const (
	PROGRESS_UPDATE packetType = "progress"
	USERS_UPDATE    packetType = "users"
)

type Packet struct {
	Type packetType `json:"type"`
	Data any        `json:"data"`
}

type ProgressUpdate struct {
	Queue       int       `json:"queue"`
	Progress    float64   `json:"progress"`
	ETA         int       `json:"eta"`
	Description string    `json:"description"`
	LastActive  time.Time `json:"last_active"`
}

type UsersUpdate struct {
	Users    int `json:"users"`
	Sessions int `json:"sessions"`
}

type subscriber struct {
	ch chan Packet
	ip string
}

type requestInit struct {
	ch        chan Packet
	stateType packetType
}

type broker struct {
	ips         map[string]int
	subscribers map[chan Packet]struct{}
	broadcast   chan Packet
	addSub      chan subscriber
	delSub      chan subscriber
	reqInit     chan requestInit
	state       map[packetType]any
}

type progress struct {
	Progress    float64 `json:"progress"`
	EtaRelative float64 `json:"eta_relative"`
	State       struct {
		JobTimestamp  string `json:"job_timestamp"`
		Job           string `json:"job"`
		JobCount      int    `json:"job_count"`
		SamplingSteps int    `json:"sampling_steps"`
		SamplingStep  int    `json:"sampling_step"`
	}
}

var b broker

func (b *broker) updateUsers() {
	b.broadcast <- Packet{Type: USERS_UPDATE, Data: UsersUpdate{Users: len(b.ips), Sessions: len(b.subscribers)}}
}

func (b *broker) start(ctx context.Context) {
	b.addSub = make(chan subscriber, 100)
	b.delSub = make(chan subscriber, 100)
	b.broadcast = make(chan Packet, 100)
	b.reqInit = make(chan requestInit)
	b.subscribers = make(map[chan Packet]struct{})
	b.ips = make(map[string]int)
	b.state = make(map[packetType]any)
	for {
		select {
		case <-ctx.Done():
			return
		case sub := <-b.addSub:
			b.subscribers[sub.ch] = struct{}{}
			b.ips[sub.ip]++
			b.updateUsers()
		case sub := <-b.delSub:
			delete(b.subscribers, sub.ch)
			b.ips[sub.ip]--
			if b.ips[sub.ip] == 0 {
				delete(b.ips, sub.ip)
			}
			b.updateUsers()
		case p := <-b.broadcast:
			b.state[p.Type] = p.Data
			for k := range b.subscribers {
				select {
				case k <- p:
				default:
					log.Printf("Message %v dropped because channel is full", p)
				}
			}
		case ri := <-b.reqInit:
			if data, ok := b.state[ri.stateType]; ok {
				p := Packet{Type: ri.stateType, Data: data}
				select {
				case ri.ch <- p:
				default:
					log.Printf("Init packet %v dropped because channel is full", p)
				}
			}
		}
	}
}

func wsHandler(c echo.Context) error {
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

func updater() {
	client := http.Client{Timeout: time.Second * 5}
	lastProgress := float64(0)
	for {
		time.Sleep(time.Second)
		resp, err := client.Get("http://stablediff-cuda:7860/sdapi/v1/progress")
		if err != nil {
			log.Printf("Error getting data: %v", err)
			continue
		}
		var p progress
		json.NewDecoder(resp.Body).Decode(&p)
		if lastProgress != p.Progress {
			b.broadcast <- Packet{
				Type: PROGRESS_UPDATE,
				Data: ProgressUpdate{
					Queue:       p.State.JobCount,
					Progress:    p.Progress,
					ETA:         int(p.EtaRelative),
					Description: fmt.Sprintf("%s %d/%d steps", "rendering", p.State.SamplingStep, p.State.SamplingSteps),
					LastActive:  time.Now(),
				}}
			lastProgress = p.Progress
		}
	}
}

func AddHandlers(e *echo.Group) {
	root, err := fs.Sub(webroot, "webroot")
	if err != nil {
		log.Fatal(err)
	}
	e.GET("/*", echo.StaticDirectoryHandler(root, false))
	e.GET("/ws", wsHandler)
	go b.start(context.Background())
	go updater()
}