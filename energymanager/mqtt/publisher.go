package mqtt

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"github.com/diebietse/invertergui/energymanager/core"
)

// Publisher publishes energy data to MQTT
// This is a minimal MQTT 3.1.1 implementation to avoid external dependencies
type Publisher struct {
	hub      *core.Hub
	broker   string
	topic    string
	clientID string
	username string
	password string
	conn     net.Conn
	mu       sync.Mutex
	stop     chan struct{}
	wg       sync.WaitGroup
}

// Config for MQTT publisher
type Config struct {
	Broker   string // tcp://host:port
	Topic    string
	ClientID string
	Username string
	Password string
}

// New creates a new MQTT publisher
func New(hub *core.Hub, cfg Config) *Publisher {
	if cfg.Topic == "" {
		cfg.Topic = "energy/status"
	}
	if cfg.ClientID == "" {
		cfg.ClientID = "energymanager"
	}
	return &Publisher{
		hub:      hub,
		broker:   cfg.Broker,
		topic:    cfg.Topic,
		clientID: cfg.ClientID,
		username: cfg.Username,
		password: cfg.Password,
		stop:     make(chan struct{}),
	}
}

// Start begins publishing
func (p *Publisher) Start() error {
	if err := p.connect(); err != nil {
		return err
	}
	p.wg.Add(1)
	go p.publishLoop()
	return nil
}

// Stop stops publishing
func (p *Publisher) Stop() {
	close(p.stop)
	p.wg.Wait()
	p.mu.Lock()
	if p.conn != nil {
		p.conn.Close()
	}
	p.mu.Unlock()
}

func (p *Publisher) connect() error {
	// Parse broker address (expect tcp://host:port or just host:port)
	addr := p.broker
	if len(addr) > 6 && addr[:6] == "tcp://" {
		addr = addr[6:]
	}
	if _, _, err := net.SplitHostPort(addr); err != nil {
		addr = addr + ":1883"
	}

	conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}

	// Send CONNECT packet
	connectPkt := p.buildConnectPacket()
	if _, err := conn.Write(connectPkt); err != nil {
		conn.Close()
		return fmt.Errorf("write connect: %w", err)
	}

	// Read CONNACK
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 4)
	if _, err := conn.Read(buf); err != nil {
		conn.Close()
		return fmt.Errorf("read connack: %w", err)
	}
	if buf[0] != 0x20 || buf[3] != 0x00 {
		conn.Close()
		return fmt.Errorf("connect rejected: %x", buf)
	}

	p.mu.Lock()
	p.conn = conn
	p.mu.Unlock()

	log.Printf("mqtt: connected to %s", addr)
	return nil
}

func (p *Publisher) publishLoop() {
	defer p.wg.Done()

	sub := p.hub.Subscribe()
	defer sub.Unsubscribe()

	pingTicker := time.NewTicker(30 * time.Second)
	defer pingTicker.Stop()

	for {
		select {
		case <-p.stop:
			return
		case info := <-sub.C():
			if info != nil {
				p.publish(info)
			}
		case <-pingTicker.C:
			p.ping()
		}
	}
}

func (p *Publisher) publish(info *core.EnergyInfo) {
	data, err := json.Marshal(info)
	if err != nil {
		log.Printf("mqtt: marshal error: %v", err)
		return
	}

	pkt := p.buildPublishPacket(p.topic, data)

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.conn == nil {
		return
	}

	p.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := p.conn.Write(pkt); err != nil {
		log.Printf("mqtt: publish error: %v", err)
		p.conn.Close()
		p.conn = nil
		// Try to reconnect
		go func() {
			time.Sleep(5 * time.Second)
			p.connect()
		}()
	}
}

func (p *Publisher) ping() {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.conn == nil {
		return
	}

	// PINGREQ packet
	p.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	p.conn.Write([]byte{0xC0, 0x00})
}

func (p *Publisher) buildConnectPacket() []byte {
	// Variable header
	varHeader := []byte{
		0x00, 0x04, 'M', 'Q', 'T', 'T', // Protocol name
		0x04,       // Protocol level (3.1.1)
		0x02,       // Connect flags (clean session)
		0x00, 0x3C, // Keep alive (60 seconds)
	}

	// Set auth flags if username/password provided
	if p.username != "" {
		varHeader[7] |= 0x80 // Username flag
		if p.password != "" {
			varHeader[7] |= 0x40 // Password flag
		}
	}

	// Payload
	payload := encodeString(p.clientID)
	if p.username != "" {
		payload = append(payload, encodeString(p.username)...)
		if p.password != "" {
			payload = append(payload, encodeString(p.password)...)
		}
	}

	// Build packet
	remainingLen := len(varHeader) + len(payload)
	pkt := []byte{0x10} // CONNECT
	pkt = append(pkt, encodeRemainingLength(remainingLen)...)
	pkt = append(pkt, varHeader...)
	pkt = append(pkt, payload...)

	return pkt
}

func (p *Publisher) buildPublishPacket(topic string, payload []byte) []byte {
	// Variable header: topic name
	varHeader := encodeString(topic)

	// Build packet
	remainingLen := len(varHeader) + len(payload)
	pkt := []byte{0x30} // PUBLISH, QoS 0
	pkt = append(pkt, encodeRemainingLength(remainingLen)...)
	pkt = append(pkt, varHeader...)
	pkt = append(pkt, payload...)

	return pkt
}

func encodeString(s string) []byte {
	b := make([]byte, 2+len(s))
	b[0] = byte(len(s) >> 8)
	b[1] = byte(len(s))
	copy(b[2:], s)
	return b
}

func encodeRemainingLength(length int) []byte {
	var encoded []byte
	for {
		b := byte(length % 128)
		length /= 128
		if length > 0 {
			b |= 0x80
		}
		encoded = append(encoded, b)
		if length == 0 {
			break
		}
	}
	return encoded
}
