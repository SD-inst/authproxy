package events

import (
	"context"
	"log"
	"time"
)

type subscriber struct {
	ch chan Packet
	ip string
}

type requestInit struct {
	ch        chan Packet
	stateType packetType
}

type Broker struct {
	ips         map[string]int
	subscribers map[chan Packet]struct{}
	broadcast   chan Packet
	addSub      chan subscriber
	delSub      chan subscriber
	reqInit     chan requestInit
	state       map[packetType]any
}

func NewBroker() *Broker {
	return &Broker{ips: map[string]int{}, subscribers: map[chan Packet]struct{}{}, broadcast: make(chan Packet, 100), addSub: make(chan subscriber, 100), delSub: make(chan subscriber, 100), reqInit: make(chan requestInit), state: map[packetType]any{}}
}

func (b *Broker) Broadcast(p Packet) {
	b.broadcast <- p
}

func (b *Broker) updateUsers() {
	b.broadcast <- Packet{Type: USERS_UPDATE, Data: UsersUpdate{Users: len(b.ips), Sessions: len(b.subscribers)}}
}

func (b *Broker) Start(ctx context.Context) {
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
			if !p.Ephemeral {
				b.state[p.Type] = p.Data
			}
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

func (b *Broker) State(s packetType) any {
	resp := make(chan Packet)
	b.reqInit <- requestInit{stateType: s, ch: resp}
	select {
	case r := <-resp:
		return r
	case <-time.After(time.Second):
		return nil
	}
}
