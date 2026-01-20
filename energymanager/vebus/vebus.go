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
		timeout:  500 * time.Millisecond,
		maxPower: maxPower,
	}
}

// Connect initializes the connection to the inverter
func (v *VEBus) Connect() error {
	v.mu.Lock()
	defer v.mu.Unlock()

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

	// Accept either response format
	rx, err := v.receiveFrame([]byte{0xFF, 0x58}, v.timeout)
	if err != nil {
		return err
	}
	if len(rx) < 4 {
		return fmt.Errorf("short response")
	}

	// Find 0x87 response code
	for i := 0; i < len(rx)-1; i++ {
		if rx[i] == 0x58 && rx[i+1] == 0x87 {
			return nil
		}
	}
	// Also check standard position
	if rx[3] == 0x87 {
		return nil
	}

	return fmt.Errorf("set power failed, response: %x", rx)
}

func (v *VEBus) sendFrame(cmd byte, data []byte) error {
	frame := v.buildFrame(cmd, data)
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
		v.rxBuf = append(v.rxBuf, buf[:n]...)

		// Look for header
		idx := bytes.Index(v.rxBuf, header)
		if idx < 0 {
			continue
		}

		// Check if we have complete frame
		if idx < len(v.rxBuf) {
			frameLen := int(v.rxBuf[idx]) + 2
			if len(v.rxBuf)-idx >= frameLen {
				frame := make([]byte, frameLen)
				copy(frame, v.rxBuf[idx:idx+frameLen])
				return frame, nil
			}
		}
	}

	if len(v.rxBuf) > 0 {
		return nil, fmt.Errorf("invalid frame: %x", v.rxBuf)
	}
	return nil, fmt.Errorf("receive timeout")
}
