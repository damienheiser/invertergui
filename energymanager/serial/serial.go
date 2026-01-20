// Package serial provides cross-platform serial port access for VE.Bus and VE.Direct communication
package serial

import (
	"io"
	"time"
)

// Config holds serial port configuration
type Config struct {
	Name        string        // Device path (e.g., /dev/ttyUSB0)
	Baud        int           // Baud rate
	ReadTimeout time.Duration // Read timeout
	Size        byte          // Data bits (default 8)
	Parity      Parity        // Parity mode
	StopBits    StopBits      // Stop bits
}

// Parity represents parity mode
type Parity byte

const (
	ParityNone  Parity = 'N'
	ParityOdd   Parity = 'O'
	ParityEven  Parity = 'E'
	ParityMark  Parity = 'M'
	ParitySpace Parity = 'S'
)

// StopBits represents stop bits configuration
type StopBits byte

const (
	Stop1     StopBits = 1
	Stop1Half StopBits = 15
	Stop2     StopBits = 2
)

// Port represents an open serial port
type Port interface {
	io.ReadWriteCloser
	SetReadDeadline(t time.Time) error
}

// OpenPort opens a serial port with the specified configuration
func OpenPort(c *Config) (Port, error) {
	return openPort(c)
}
