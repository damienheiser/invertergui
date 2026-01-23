package vebus

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"sync"
	"time"
)

// Device state IDs
const (
	StateDown       = 0
	StateStartup    = 1
	StateOff        = 2
	StateSlave      = 3
	StateInvertFull = 4
	StateInvertHalf = 5
	StateInvertAES  = 6
	StatePowerAsst  = 7
	StateBypass     = 8
	StateCharge     = 9
)

var stateNames = map[int]string{
	StateDown:       "down",
	StateStartup:    "startup",
	StateOff:        "off",
	StateSlave:      "slave",
	StateInvertFull: "invert_full",
	StateInvertHalf: "invert_half",
	StateInvertAES:  "invert_aes",
	StatePowerAsst:  "power_assist",
	StateBypass:     "bypass",
	StateCharge:     "charge",
}

// LED bit positions
const (
	LEDMains       = 1 << 0
	LEDAbsorption  = 1 << 1
	LEDBulk        = 1 << 2
	LEDFloat       = 1 << 3
	LEDInverter    = 1 << 4
	LEDOverload    = 1 << 5
	LEDLowBat      = 1 << 6
	LEDTemperature = 1 << 7
)

// VEBus communicates with Victron devices via MK2/MK3 interface
type VEBus struct {
	port            io.ReadWriteCloser
	mu              sync.Mutex
	essSetpointRAM  int // RAM ID for ESS setpoint (discovered by scan)
	rxBuf           []byte
	timeout         time.Duration
	maxPower        float64 // Maximum charge/discharge power in watts
}

// Status contains all data read from the inverter
type Status struct {
	MK2Version     uint32
	DeviceState    int
	DeviceStateName string
	LEDLight       byte
	LEDBlink       byte
	MainsVoltage   float64 // V
	MainsCurrent   float64 // A
	InvVoltage     float64 // V
	InvCurrent     float64 // A
	InvPower       float64 // W (negative = feeding to grid)
	OutPower       float64 // W
	BatVoltage     float64 // V
	BatCurrent     float64 // A
	BatPower       float64 // W
	BatSOC         float64 // 0-100%
	Valid          bool
	Error          string
}

// New creates a new VEBus connection
func New(port io.ReadWriteCloser, maxPower float64) *VEBus {
	if maxPower <= 0 {
		maxPower = 5000
	}
	return &VEBus{
		port:     port,
		rxBuf:    make([]byte, 0, 256),
		timeout:  2 * time.Second, // Increased for MK3 response time
		maxPower: maxPower,
	}
}

// Connect initializes the connection to the inverter
func (v *VEBus) Connect() error {
	v.mu.Lock()
	defer v.mu.Unlock()

	// Give the MK3 time to initialize after port open
	log.Println("vebus: waiting 200ms for MK3 to initialize...")
	time.Sleep(200 * time.Millisecond)

	log.Println("vebus: requesting MK2 version...")
	// Get version
	version, err := v.getVersion()
	if err != nil {
		return fmt.Errorf("get version: %w", err)
	}
	log.Printf("vebus: MK2 version: %d", version)

	// Init address
	if err := v.initAddress(0); err != nil {
		return fmt.Errorf("init address: %w", err)
	}

	// Scan for ESS assistant
	ramID, err := v.scanESSAssistant()
	if err != nil {
		return fmt.Errorf("scan ESS: %w", err)
	}
	v.essSetpointRAM = ramID
	log.Printf("vebus: ESS setpoint RAM ID: %d", ramID)

	return nil
}

// Read returns current inverter status
func (v *VEBus) Read() (*Status, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	status := &Status{Valid: true}

	// Request snapshot
	if err := v.sendSnapshotRequest(); err != nil {
		status.Valid = false
		status.Error = err.Error()
		return status, nil
	}
	time.Sleep(100 * time.Millisecond)

	// Get AC info
	ac, err := v.getACInfo()
	if err != nil {
		status.Valid = false
		status.Error = err.Error()
		return status, nil
	}
	status.DeviceState = ac.state
	status.DeviceStateName = ac.stateName
	status.MainsVoltage = ac.mainsVoltage
	status.MainsCurrent = ac.mainsCurrent
	status.InvVoltage = ac.invVoltage
	status.InvCurrent = ac.invCurrent

	time.Sleep(100 * time.Millisecond)

	// Read snapshot
	snap, err := v.readSnapshot()
	if err != nil {
		status.Valid = false
		status.Error = err.Error()
		return status, nil
	}
	status.InvPower = snap.invPower
	status.OutPower = snap.outPower
	status.BatVoltage = snap.batVoltage
	status.BatCurrent = snap.batCurrent
	status.BatPower = snap.batPower
	status.BatSOC = snap.soc

	time.Sleep(100 * time.Millisecond)

	// Get LED status
	led, err := v.getLED()
	if err != nil {
		status.Valid = false
		status.Error = err.Error()
		return status, nil
	}
	status.LEDLight = led.light
	status.LEDBlink = led.blink

	return status, nil
}

// SetPower sets the ESS power setpoint
// Positive = charge from grid, Negative = feed to grid
func (v *VEBus) SetPower(watts float64) error {
	v.mu.Lock()
	defer v.mu.Unlock()

	if v.essSetpointRAM == 0 {
		return fmt.Errorf("ESS assistant not initialized")
	}

	// Clamp to max power
	if watts > v.maxPower {
		watts = v.maxPower
	}
	if watts < -v.maxPower {
		watts = -v.maxPower
	}

	return v.setPowerInternal(int16(-watts)) // Negate for VE.Bus convention
}

// Wakeup sends wakeup command
func (v *VEBus) Wakeup() error {
	v.mu.Lock()
	defer v.mu.Unlock()
	_, err := v.port.Write([]byte{0x05, 0x3F, 0x07, 0x00, 0x00, 0x00, 0xC2})
	return err
}

// Sleep sends sleep command
func (v *VEBus) Sleep() error {
	v.mu.Lock()
	defer v.mu.Unlock()
	_, err := v.port.Write([]byte{0x05, 0x3F, 0x04, 0x00, 0x00, 0x00, 0xC5})
	return err
}

// Close closes the connection
func (v *VEBus) Close() error {
	return v.port.Close()
}

// Internal methods

func (v *VEBus) getVersion() (uint32, error) {
	if err := v.sendFrame('V', nil); err != nil {
		return 0, err
	}
	rx, err := v.receiveFrame([]byte{0x07, 0xFF}, v.timeout)
	if err != nil {
		return 0, err
	}
	if len(rx) < 7 {
		return 0, fmt.Errorf("short response")
	}
	version := binary.LittleEndian.Uint32(rx[3:7])
	return version, nil
}

func (v *VEBus) initAddress(addr byte) error {
	if err := v.sendFrame('A', []byte{0x01, addr}); err != nil {
		return err
	}
	rx, err := v.receiveFrame([]byte{0x04, 0xFF, 0x41}, v.timeout)
	if err != nil {
		return err
	}
	if len(rx) < 5 || rx[4] != addr {
		return fmt.Errorf("address init failed")
	}
	return nil
}

type ledInfo struct {
	light byte
	blink byte
}

func (v *VEBus) getLED() (*ledInfo, error) {
	if err := v.sendFrame('L', nil); err != nil {
		return nil, err
	}
	rx, err := v.receiveFrame([]byte{0x08, 0xFF, 0x4C}, v.timeout)
	if err != nil {
		return nil, err
	}
	if len(rx) < 5 {
		return nil, fmt.Errorf("short response")
	}
	return &ledInfo{
		light: rx[3],
		blink: rx[4],
	}, nil
}

type acInfo struct {
	state        int
	stateName    string
	mainsVoltage float64
	mainsCurrent float64
	invVoltage   float64
	invCurrent   float64
}

func (v *VEBus) getACInfo() (*acInfo, error) {
	if err := v.sendFrame('F', []byte{0x01}); err != nil {
		return nil, err
	}
	rx, err := v.receiveFrame([]byte{0x0F, 0x20}, v.timeout)
	if err != nil {
		return nil, err
	}
	if len(rx) < 16 {
		return nil, fmt.Errorf("short response")
	}

	// Parse: <BFfactor> <InvFactor> <skip> <DeviceState> <PhaseInfo> <MainsU> <MainsI> <InvU> <InvI> <Period>
	state := int(rx[5])
	mainsU := int16(binary.LittleEndian.Uint16(rx[7:9]))
	mainsI := int16(binary.LittleEndian.Uint16(rx[9:11]))
	invU := int16(binary.LittleEndian.Uint16(rx[11:13]))
	invI := int16(binary.LittleEndian.Uint16(rx[13:15]))

	stateName := stateNames[state]
	if stateName == "" {
		stateName = fmt.Sprintf("unknown_%d", state)
	}

	return &acInfo{
		state:        state,
		stateName:    stateName,
		mainsVoltage: float64(mainsU) / 100,
		mainsCurrent: float64(mainsI) / 100,
		invVoltage:   float64(invU) / 100,
		invCurrent:   float64(invI) / 100,
	}, nil
}

func (v *VEBus) sendSnapshotRequest() error {
	// Request snapshot of: InvPower(15), OutPower(16), UBat(4), IBat(5), SOC(13)
	return v.sendFrame('F', []byte{0x06, 15, 16, 4, 5, 13})
}

type snapshot struct {
	invPower   float64
	outPower   float64
	batVoltage float64
	batCurrent float64
	batPower   float64
	soc        float64
}

func (v *VEBus) readSnapshot() (*snapshot, error) {
	if err := v.sendFrame('X', []byte{0x38}); err != nil {
		return nil, err
	}
	rx, err := v.receiveFrame([]byte{0x0D, 0xFF, 0x58}, v.timeout)
	if err != nil {
		return nil, err
	}
	if len(rx) < 14 || rx[3] != 0x99 {
		return nil, fmt.Errorf("invalid snapshot response")
	}

	invP := int16(binary.LittleEndian.Uint16(rx[4:6]))
	outP := int16(binary.LittleEndian.Uint16(rx[6:8]))
	batU := int16(binary.LittleEndian.Uint16(rx[8:10]))
	batI := int16(binary.LittleEndian.Uint16(rx[10:12]))
	soc := int16(binary.LittleEndian.Uint16(rx[12:14]))

	batVoltage := float64(batU) / 100
	batCurrent := float64(batI) / 10

	return &snapshot{
		invPower:   float64(-invP), // Negate for standard convention
		outPower:   float64(outP),
		batVoltage: batVoltage,
		batCurrent: batCurrent,
		batPower:   batVoltage * batCurrent,
		soc:        float64(soc) / 2,
	}, nil
}

func (v *VEBus) scanESSAssistant() (int, error) {
	ramID := 128
	for n := 0; n < 8; n++ {
		data := make([]byte, 3)
		data[0] = 0x30 // ReadRAMVar command
		binary.LittleEndian.PutUint16(data[1:3], uint16(ramID))

		if err := v.sendFrame('X', data); err != nil {
			return 0, err
		}
		rx, err := v.receiveFrame([]byte{0x07, 0xFF, 0x58}, v.timeout)
		if err != nil {
			return 0, err
		}
		if len(rx) < 6 {
			return 0, fmt.Errorf("short response")
		}

		ramVal := uint16(rx[4]) | uint16(rx[5])<<8
		log.Printf("vebus: scan ramID=%d value=0x%04X", ramID, ramVal)

		if ramVal&0xFFF0 == 0x0050 { // ESS Assistant marker
			return ramID + 1, nil // Setpoint is at next RAM ID
		}
		ramID += 1 + int(ramVal&0x000F) // Skip to next assistant
	}
	return 0, fmt.Errorf("ESS assistant not found")
}

func (v *VEBus) setPowerInternal(power int16) error {
	data := make([]byte, 5)
	data[0] = 0x37 // WriteViaID command
	data[1] = 0x02 // Flags: RAM only (no EEPROM)
	data[2] = byte(v.essSetpointRAM)
	binary.LittleEndian.PutUint16(data[3:5], uint16(power))

	if err := v.sendFrame('X', data); err != nil {
		return err
	}

	// Response is 0x03 0xFF 0x58 0x87 [checksum] (length=3 bytes after header)
	// Use header with length byte to ensure proper frame parsing
	rx, err := v.receiveFrame([]byte{0x03, 0xFF, 0x58}, v.timeout)
	if err != nil {
		return err
	}
	if len(rx) < 4 {
		return fmt.Errorf("short response: %x", rx)
	}

	// Check for 0x87 success response code at position 3
	if rx[3] == 0x87 {
		return nil
	}

	return fmt.Errorf("set power failed, response: %x", rx)
}

func (v *VEBus) sendFrame(cmd byte, data []byte) error {
	frame := v.buildFrame(cmd, data)
	log.Printf("vebus: TX cmd=%c data=%x frame=%x", cmd, data, frame)
	_, err := v.port.Write(frame)
	return err
}

func (v *VEBus) buildFrame(cmd byte, data []byte) []byte {
	length := len(data) + 2
	frame := make([]byte, length+2)
	frame[0] = byte(length)
	frame[1] = 0xFF
	frame[2] = cmd
	copy(frame[3:], data)

	// Calculate checksum
	var sum byte
	for i := 0; i < len(frame)-1; i++ {
		sum += frame[i]
	}
	frame[len(frame)-1] = byte(256 - int(sum))
	return frame
}

func (v *VEBus) receiveFrame(header []byte, timeout time.Duration) ([]byte, error) {
	deadline := time.Now().Add(timeout)
	v.rxBuf = v.rxBuf[:0]
	buf := make([]byte, 64)

	for time.Now().Before(deadline) {
		// Set read deadline
		if setter, ok := v.port.(interface{ SetReadDeadline(time.Time) error }); ok {
			setter.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
		}

		n, err := v.port.Read(buf)
		if err != nil && n == 0 {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		if n > 0 {
			log.Printf("vebus: RX +%d bytes: %x", n, buf[:n])
		}
		v.rxBuf = append(v.rxBuf, buf[:n]...)

		// Try to find and extract valid frames from buffer
		for len(v.rxBuf) >= 2 {
			// Look for any valid frame start (length byte followed by 0xFF or 0x20 marker)
			frameStart := -1
			for i := 0; i < len(v.rxBuf)-1; i++ {
				if v.rxBuf[i] >= 2 && v.rxBuf[i] <= 32 { // Reasonable length 2-32
					if v.rxBuf[i+1] == 0xFF || v.rxBuf[i+1] == 0x20 {
						frameStart = i
						break
					}
				}
			}

			if frameStart < 0 {
				// No valid frame start found, keep only last byte (might be start of next frame)
				if len(v.rxBuf) > 1 {
					v.rxBuf = v.rxBuf[len(v.rxBuf)-1:]
				}
				break
			}

			// Discard garbage before frame start
			if frameStart > 0 {
				log.Printf("vebus: discarding %d bytes of garbage: %x", frameStart, v.rxBuf[:frameStart])
				v.rxBuf = v.rxBuf[frameStart:]
			}

			// Check if we have complete frame
			frameLen := int(v.rxBuf[0]) + 2 // length byte + data + checksum
			if len(v.rxBuf) < frameLen {
				break // Need more data
			}

			// Extract frame
			frame := make([]byte, frameLen)
			copy(frame, v.rxBuf[:frameLen])
			v.rxBuf = v.rxBuf[frameLen:]

			// Check if this frame matches our expected header
			if len(header) <= len(frame) && bytes.Equal(frame[:len(header)], header) {
				log.Printf("vebus: RX complete frame: %x", frame)
				return frame, nil
			}

			// Frame doesn't match our header - it's an unsolicited frame, skip it
			log.Printf("vebus: skipping unsolicited frame: %x (waiting for %x)", frame, header)
		}
	}

	if len(v.rxBuf) > 0 {
		log.Printf("vebus: RX timeout with partial data: %x (waiting for header %x)", v.rxBuf, header)
		return nil, fmt.Errorf("receive timeout with partial data")
	}
	log.Printf("vebus: RX timeout (no data received)")
	return nil, fmt.Errorf("receive timeout")
}
