package core

import (
	"sync"
)

// Hub broadcasts EnergyInfo to all subscribers
type Hub struct {
	mu          sync.RWMutex
	subscribers map[*Subscription]bool
	register    chan *Subscription
	unregister  chan *Subscription
	broadcast   chan *EnergyInfo
	done        chan struct{}
	latest      *EnergyInfo
}

// Subscription receives EnergyInfo updates
type Subscription struct {
	send chan *EnergyInfo
	hub  *Hub
}

// NewHub creates a new broadcast hub
func NewHub() *Hub {
	h := &Hub{
		subscribers: make(map[*Subscription]bool),
		register:    make(chan *Subscription, 16),
		unregister:  make(chan *Subscription, 16),
		broadcast:   make(chan *EnergyInfo, 16),
		done:        make(chan struct{}),
	}
	go h.run()
	return h
}

func (h *Hub) run() {
	for {
		select {
		case sub := <-h.register:
			h.mu.Lock()
			h.subscribers[sub] = true
			h.mu.Unlock()
		case sub := <-h.unregister:
			h.mu.Lock()
			if _, ok := h.subscribers[sub]; ok {
				delete(h.subscribers, sub)
				close(sub.send)
			}
			h.mu.Unlock()
		case info := <-h.broadcast:
			h.mu.Lock()
			h.latest = info
			for sub := range h.subscribers {
				select {
				case sub.send <- info:
				default:
					// Drop if subscriber is slow
				}
			}
			h.mu.Unlock()
		case <-h.done:
			h.mu.Lock()
			for sub := range h.subscribers {
				close(sub.send)
			}
			h.mu.Unlock()
			return
		}
	}
}

// Subscribe creates a new subscription
func (h *Hub) Subscribe() *Subscription {
	sub := &Subscription{
		send: make(chan *EnergyInfo, 8),
		hub:  h,
	}
	h.register <- sub
	return sub
}

// Broadcast sends info to all subscribers
func (h *Hub) Broadcast(info *EnergyInfo) {
	select {
	case h.broadcast <- info:
	default:
	}
}

// Latest returns the most recent EnergyInfo
func (h *Hub) Latest() *EnergyInfo {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.latest
}

// Close shuts down the hub
func (h *Hub) Close() {
	close(h.done)
}

// C returns the channel for receiving updates
func (s *Subscription) C() <-chan *EnergyInfo {
	return s.send
}

// Unsubscribe removes this subscription
func (s *Subscription) Unsubscribe() {
	s.hub.unregister <- s
}
