package webui

import (
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/diebietse/invertergui/energymanager/core"
	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		return true // Allow all origins for local use
	},
}

// WSHub manages WebSocket connections and broadcasts
type WSHub struct {
	clients    map[*WSClient]bool
	broadcast  chan *core.EnergyInfo
	register   chan *WSClient
	unregister chan *WSClient
	mu         sync.RWMutex
	hub        *core.Hub
	stopCh     chan struct{}
	wg         sync.WaitGroup
}

// WSClient represents a connected WebSocket client
type WSClient struct {
	hub    *WSHub
	conn   *websocket.Conn
	send   chan []byte
	closed bool
	mu     sync.Mutex
}

// NewWSHub creates a new WebSocket hub
func NewWSHub(hub *core.Hub) *WSHub {
	return &WSHub{
		clients:    make(map[*WSClient]bool),
		broadcast:  make(chan *core.EnergyInfo, 16),
		register:   make(chan *WSClient),
		unregister: make(chan *WSClient),
		hub:        hub,
		stopCh:     make(chan struct{}),
	}
}

// Start begins the WebSocket hub
func (h *WSHub) Start() {
	h.wg.Add(2)
	go h.run()
	go h.subscribeLoop()
	log.Println("websocket: hub started")
}

// Stop shuts down the WebSocket hub
func (h *WSHub) Stop() {
	close(h.stopCh)
	h.wg.Wait()
	log.Println("websocket: hub stopped")
}

// ClientCount returns the number of connected clients
func (h *WSHub) ClientCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

func (h *WSHub) run() {
	defer h.wg.Done()

	for {
		select {
		case <-h.stopCh:
			// Close all client connections
			h.mu.Lock()
			for client := range h.clients {
				close(client.send)
				delete(h.clients, client)
			}
			h.mu.Unlock()
			return

		case client := <-h.register:
			h.mu.Lock()
			h.clients[client] = true
			h.mu.Unlock()
			log.Printf("websocket: client connected (total: %d)", len(h.clients))

		case client := <-h.unregister:
			h.mu.Lock()
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				close(client.send)
			}
			h.mu.Unlock()
			log.Printf("websocket: client disconnected (total: %d)", len(h.clients))

		case info := <-h.broadcast:
			data, err := json.Marshal(info)
			if err != nil {
				log.Printf("websocket: marshal error: %v", err)
				continue
			}

			h.mu.RLock()
			for client := range h.clients {
				select {
				case client.send <- data:
				default:
					// Client buffer full, skip this message
				}
			}
			h.mu.RUnlock()
		}
	}
}

func (h *WSHub) subscribeLoop() {
	defer h.wg.Done()

	sub := h.hub.Subscribe()
	defer sub.Unsubscribe()

	for {
		select {
		case <-h.stopCh:
			return
		case info := <-sub.C():
			if info != nil {
				select {
				case h.broadcast <- info:
				default:
					// Broadcast channel full, skip
				}
			}
		}
	}
}

// ServeWS handles WebSocket requests
func (h *WSHub) ServeWS(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("websocket: upgrade error: %v", err)
		return
	}

	client := &WSClient{
		hub:  h,
		conn: conn,
		send: make(chan []byte, 64),
	}

	h.register <- client

	go client.writePump()
	go client.readPump()
}

func (c *WSClient) readPump() {
	defer func() {
		c.hub.unregister <- c
		c.conn.Close()
	}()

	c.conn.SetReadLimit(512)
	c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	c.conn.SetPongHandler(func(string) error {
		c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})

	for {
		_, _, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("websocket: read error: %v", err)
			}
			break
		}
	}
}

func (c *WSClient) writePump() {
	ticker := time.NewTicker(30 * time.Second)
	defer func() {
		ticker.Stop()
		c.conn.Close()
	}()

	for {
		select {
		case message, ok := <-c.send:
			c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if !ok {
				c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			if err := c.conn.WriteMessage(websocket.TextMessage, message); err != nil {
				return
			}

		case <-ticker.C:
			c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}
