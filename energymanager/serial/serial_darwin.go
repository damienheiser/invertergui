//go:build darwin

package serial

import (
	"fmt"
	"os"
	"syscall"
	"time"
	"unsafe"
)

// termios structure for Darwin
type termios struct {
	Iflag  uint64
	Oflag  uint64
	Cflag  uint64
	Lflag  uint64
	Cc     [20]uint8
	Ispeed uint64
	Ospeed uint64
}

// ioctl constants for Darwin
const (
	tcgets  = 0x40487413 // TIOCGETA
	tcsetsw = 0x80487415 // TIOCSETAW
)

// c_cflag bits (Darwin uses different values)
const (
	csize  = 0x300
	cs5    = 0x0
	cs6    = 0x100
	cs7    = 0x200
	cs8    = 0x300
	cstopb = 0x400
	cread  = 0x800
	parenb = 0x1000
	parodd = 0x2000
	hupcl  = 0x4000
	clocal = 0x8000
)

// c_lflag bits
const (
	isig   = 0x80
	icanon = 0x100
	echo   = 0x8
	iexten = 0x400
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
	ixon   = 0x200
	ixoff  = 0x400
	ixany  = 0x800
)

// c_oflag bits
const (
	opost = 0x1
)

// Baud rate values for Darwin
var baudRates = map[int]uint64{
	50:     50,
	75:     75,
	110:    110,
	134:    134,
	150:    150,
	200:    200,
	300:    300,
	600:    600,
	1200:   1200,
	1800:   1800,
	2400:   2400,
	4800:   4800,
	9600:   9600,
	19200:  19200,
	38400:  38400,
	57600:  57600,
	115200: 115200,
	230400: 230400,
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
	t.Iflag &^= ignbrk | brkint | parmrk | istrip | inlcr | igncr | icrnl | ixon | ixoff | ixany
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
	t.Cc[16] = 1 // VMIN
	if c.ReadTimeout > 0 {
		vtime := uint8(c.ReadTimeout.Milliseconds() / 100)
		if vtime == 0 {
			vtime = 1
		}
		if vtime > 255 {
			vtime = 255
		}
		t.Cc[17] = vtime // VTIME
	} else {
		t.Cc[17] = 0 // No timeout
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
