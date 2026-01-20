//go:build linux

package serial

import (
	"fmt"
	"os"
	"syscall"
	"time"
	"unsafe"
)

// termios structure for Linux
type termios struct {
	Iflag  uint32
	Oflag  uint32
	Cflag  uint32
	Lflag  uint32
	Line   uint8
	Cc     [19]uint8
	Ispeed uint32
	Ospeed uint32
}

// ioctl constants
const (
	tcgets  = 0x5401
	tcsets  = 0x5402
	tcsetsw = 0x5403
)

// c_cflag bits
const (
	csize  = 0x30
	cs5    = 0x0
	cs6    = 0x10
	cs7    = 0x20
	cs8    = 0x30
	cstopb = 0x40
	cread  = 0x80
	parenb = 0x100
	parodd = 0x200
	hupcl  = 0x400
	clocal = 0x800
)

// c_lflag bits
const (
	isig   = 0x1
	icanon = 0x2
	echo   = 0x8
	iexten = 0x8000
)

// c_iflag bits
const (
	ignbrk = 0x1
	brkint = 0x2
	ignpar = 0x4
	parmrk = 0x8
	inpck  = 0x10
	istrip = 0x20
	inlcr  = 0x40
	igncr  = 0x80
	icrnl  = 0x100
	ixon   = 0x400
	ixany  = 0x800
	ixoff  = 0x1000
)

// c_oflag bits
const (
	opost = 0x1
)

// Baud rate constants for Linux
var baudRates = map[int]uint32{
	50:     0x1,
	75:     0x2,
	110:    0x3,
	134:    0x4,
	150:    0x5,
	200:    0x6,
	300:    0x7,
	600:    0x8,
	1200:   0x9,
	1800:   0xA,
	2400:   0xB,
	4800:   0xC,
	9600:   0xD,
	19200:  0xE,
	38400:  0xF,
	57600:  0x1001,
	115200: 0x1002,
	230400: 0x1003,
}

type port struct {
	file        *os.File
	fd          int
	readTimeout time.Duration
}

func openPort(c *Config) (Port, error) {
	// Open port
	file, err := os.OpenFile(c.Name, os.O_RDWR|syscall.O_NOCTTY|syscall.O_NONBLOCK, 0666)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", c.Name, err)
	}

	fd := int(file.Fd())

	// Clear non-blocking flag
	if err := syscall.SetNonblock(fd, false); err != nil {
		file.Close()
		return nil, fmt.Errorf("set blocking: %w", err)
	}

	// Get current settings
	var t termios
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), tcgets, uintptr(unsafe.Pointer(&t))); errno != 0 {
		file.Close()
		return nil, fmt.Errorf("get attributes: %v", errno)
	}

	// Configure raw mode
	t.Iflag &^= ignbrk | brkint | parmrk | istrip | inlcr | igncr | icrnl | ixon
	t.Oflag &^= opost
	t.Lflag &^= echo | icanon | isig | iexten
	t.Cflag &^= csize | parenb | parodd | cstopb
	t.Cflag |= cs8 | cread | clocal

	// Set data bits
	if c.Size > 0 {
		t.Cflag &^= csize
		switch c.Size {
		case 5:
			t.Cflag |= cs5
		case 6:
			t.Cflag |= cs6
		case 7:
			t.Cflag |= cs7
		case 8:
			t.Cflag |= cs8
		}
	}

	// Set parity
	switch c.Parity {
	case ParityNone:
		// Default - no change needed
	case ParityOdd:
		t.Cflag |= parenb | parodd
	case ParityEven:
		t.Cflag |= parenb
	}

	// Set stop bits
	switch c.StopBits {
	case Stop2:
		t.Cflag |= cstopb
	}

	// Set baud rate
	speed, ok := baudRates[c.Baud]
	if !ok {
		file.Close()
		return nil, fmt.Errorf("unsupported baud rate: %d", c.Baud)
	}
	t.Ispeed = speed
	t.Ospeed = speed

	// Set VMIN and VTIME
	// VMIN = 1 (wait for at least 1 byte)
	// VTIME = timeout in deciseconds
	t.Cc[6] = 1 // VMIN
	if c.ReadTimeout > 0 {
		vtime := uint8(c.ReadTimeout.Milliseconds() / 100)
		if vtime == 0 {
			vtime = 1
		}
		if vtime > 255 {
			vtime = 255
		}
		t.Cc[5] = vtime // VTIME
	} else {
		t.Cc[5] = 0 // No timeout
	}

	// Apply settings
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), tcsetsw, uintptr(unsafe.Pointer(&t))); errno != 0 {
		file.Close()
		return nil, fmt.Errorf("set attributes: %v", errno)
	}

	return &port{
		file:        file,
		fd:          fd,
		readTimeout: c.ReadTimeout,
	}, nil
}

func (p *port) Read(b []byte) (int, error) {
	return p.file.Read(b)
}

func (p *port) Write(b []byte) (int, error) {
	return p.file.Write(b)
}

func (p *port) Close() error {
	return p.file.Close()
}

func (p *port) SetReadDeadline(t time.Time) error {
	return p.file.SetReadDeadline(t)
}
