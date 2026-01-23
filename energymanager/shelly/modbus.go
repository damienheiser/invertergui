package shelly

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"net"
	"sync"
	"time"
)

// ModbusClient is a minimal Modbus TCP client
type ModbusClient struct {
	addr    string
	conn    net.Conn
	mu      sync.Mutex
	transID uint16
	timeout time.Duration
}

// NewModbusClient creates a new Modbus TCP client
func NewModbusClient(addr string, timeout time.Duration) *ModbusClient {
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	return &ModbusClient{
		addr:    addr,
		timeout: timeout,
	}
}

// Connect establishes the TCP connection
func (c *ModbusClient) Connect() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		c.conn.Close()
	}
	conn, err := net.DialTimeout("tcp", c.addr, c.timeout)
	if err != nil {
		return err
	}
	c.conn = conn
	return nil
}

// Close closes the connection
func (c *ModbusClient) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}

// ReadInputRegisters reads input registers (function code 0x04)
func (c *ModbusClient) ReadInputRegisters(unitID byte, addr uint16, count uint16) ([]uint16, error) {
	return c.readRegisters(unitID, 0x04, addr, count)
}

// ReadHoldingRegisters reads holding registers (function code 0x03)
func (c *ModbusClient) ReadHoldingRegisters(unitID byte, addr uint16, count uint16) ([]uint16, error) {
	return c.readRegisters(unitID, 0x03, addr, count)
}

func (c *ModbusClient) readRegisters(unitID byte, funcCode byte, addr uint16, count uint16) ([]uint16, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn == nil {
		return nil, fmt.Errorf("not connected")
	}

	c.transID++
	transID := c.transID

	// Build request: MBAP header + PDU
	// MBAP: Transaction ID (2) + Protocol ID (2) + Length (2) + Unit ID (1)
	// PDU: Function code (1) + Start address (2) + Quantity (2)
	req := make([]byte, 12)
	binary.BigEndian.PutUint16(req[0:2], transID)     // Transaction ID
	binary.BigEndian.PutUint16(req[2:4], 0)          // Protocol ID (Modbus)
	binary.BigEndian.PutUint16(req[4:6], 6)          // Length
	req[6] = unitID                                   // Unit ID
	req[7] = funcCode                                 // Function code
	binary.BigEndian.PutUint16(req[8:10], addr)      // Starting address
	binary.BigEndian.PutUint16(req[10:12], count)    // Quantity of registers

	c.conn.SetDeadline(time.Now().Add(c.timeout))
	_, err := c.conn.Write(req)
	if err != nil {
		return nil, err
	}

	// Read MBAP header
	header := make([]byte, 7)
	_, err = io.ReadFull(c.conn, header)
	if err != nil {
		return nil, err
	}

	respTransID := binary.BigEndian.Uint16(header[0:2])
	if respTransID != transID {
		return nil, fmt.Errorf("transaction ID mismatch: got %d, want %d", respTransID, transID)
	}

	length := binary.BigEndian.Uint16(header[4:6])
	if length < 2 {
		return nil, fmt.Errorf("invalid length: %d", length)
	}

	// Read PDU
	pdu := make([]byte, length-1) // -1 for unit ID already in header
	_, err = io.ReadFull(c.conn, pdu)
	if err != nil {
		return nil, err
	}

	// Check for exception
	if pdu[0]&0x80 != 0 {
		return nil, fmt.Errorf("modbus exception: function=%02x, code=%02x", pdu[0], pdu[1])
	}

	if pdu[0] != funcCode {
		return nil, fmt.Errorf("function code mismatch: got %02x, want %02x", pdu[0], funcCode)
	}

	byteCount := int(pdu[1])
	if byteCount != int(count)*2 {
		return nil, fmt.Errorf("byte count mismatch: got %d, want %d", byteCount, count*2)
	}

	// Parse registers
	regs := make([]uint16, count)
	for i := uint16(0); i < count; i++ {
		regs[i] = binary.BigEndian.Uint16(pdu[2+i*2 : 4+i*2])
	}

	return regs, nil
}

// ReadFloat32 reads a 32-bit float from two consecutive registers (big-endian word order)
func (c *ModbusClient) ReadFloat32(unitID byte, addr uint16) (float64, error) {
	regs, err := c.ReadInputRegisters(unitID, addr, 2)
	if err != nil {
		return 0, err
	}
	// Shelly Pro 3EM uses big-endian word order: high word first, then low word
	bits := uint32(regs[0])<<16 | uint32(regs[1])
	return float64(math.Float32frombits(bits)), nil
}

// RegistersToFloat32 converts two registers to float32 (big-endian word order)
// Shelly Pro 3EM uses high word first, then low word
func RegistersToFloat32(regs []uint16, offset int) float64 {
	if offset+1 >= len(regs) {
		return 0
	}
	// High word first, then low word (big-endian)
	bits := uint32(regs[offset])<<16 | uint32(regs[offset+1])
	return float64(math.Float32frombits(bits))
}
