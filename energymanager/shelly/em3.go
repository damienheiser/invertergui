package shelly

import (
	"log"
	"sync"
	"time"

	"github.com/diebietse/invertergui/energymanager/core"
)

// Shelly Pro 3EM Modbus Register Addresses (Input Registers, Function Code 4)
// This device uses EM1 components (not EM), each channel has its own component
// Shelly Gen2 documentation says 32000, but actual Modbus address is 2000
// All Float32 values are IEEE 754 format (2 registers each), big-endian
// Each EM1 component block is 20 registers
// Within block: timestamp(2), error(2), voltage(2), current(2), power(2), apparent(2), pf(2), errors(3), freq(2), reserved(1)
const (
	// EM1:0 (Phase A) registers - block starts at 2000
	em1_0_Base = 2000
	// EM1:1 (Phase B) registers - block starts at 2020
	em1_1_Base = 2020
	// EM1:2 (Phase C) registers - block starts at 2040
	em1_2_Base = 2040
)

// GridPowerSource defines which value to use as the grid power reading
type GridPowerSource string

const (
	GridPowerTotal   GridPowerSource = "total"    // Sum of all phases (default)
	GridPowerPhaseA  GridPowerSource = "phase_a"  // Use Phase A only
	GridPowerPhaseB  GridPowerSource = "phase_b"  // Use Phase B only
	GridPowerPhaseC  GridPowerSource = "phase_c"  // Use Phase C only (entire power station)
	GridPowerPhaseAB GridPowerSource = "phase_ab" // Sum of Phase A + Phase B (for PID)
)

// EM3 represents a Shelly Pro EM3 energy meter
type EM3 struct {
	client          *ModbusClient
	addr            string
	unitID          byte
	interval        time.Duration
	gridPowerSource GridPowerSource
	dataCh          chan *core.GridData
	stopCh          chan struct{}
	wg              sync.WaitGroup
	mu              sync.RWMutex
	connected       bool
	lastError       string
	errorCount      int
	reconnecting    bool
}

// NewEM3 creates a new Shelly Pro EM3 driver
// gridPowerSource specifies which value to use as the grid power reading
func NewEM3(addr string, pollInterval time.Duration, gridPowerSource GridPowerSource) *EM3 {
	if pollInterval == 0 {
		pollInterval = time.Second
	}
	if gridPowerSource == "" {
		gridPowerSource = GridPowerTotal
	}
	return &EM3{
		client:          NewModbusClient(addr, 5*time.Second),
		addr:            addr,
		unitID:          1, // Shelly default unit ID
		interval:        pollInterval,
		gridPowerSource: gridPowerSource,
		dataCh:          make(chan *core.GridData, 8),
		stopCh:          make(chan struct{}),
	}
}

// SetGridPowerSource changes which phase/total to use for grid power
func (e *EM3) SetGridPowerSource(source GridPowerSource) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.gridPowerSource = source
}

// GetGridPowerSource returns the current grid power source setting
func (e *EM3) GetGridPowerSource() GridPowerSource {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.gridPowerSource
}

// Connected returns true if currently connected to the Shelly
func (e *EM3) Connected() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.connected
}

// LastError returns the last error message
func (e *EM3) LastError() string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.lastError
}

// Start begins polling the meter
func (e *EM3) Start() error {
	if err := e.client.Connect(); err != nil {
		e.mu.Lock()
		e.lastError = err.Error()
		e.connected = false
		e.mu.Unlock()
		// Start polling anyway - it will attempt reconnection
		log.Printf("shelly: initial connection failed: %v (will retry)", err)
	} else {
		e.mu.Lock()
		e.connected = true
		e.lastError = ""
		e.mu.Unlock()
		log.Printf("shelly: connected to %s", e.addr)
	}
	e.wg.Add(1)
	go e.pollLoop()
	return nil
}

// Stop stops polling
func (e *EM3) Stop() {
	close(e.stopCh)
	e.wg.Wait()
	e.client.Close()
}

// C returns the channel for receiving grid data
func (e *EM3) C() <-chan *core.GridData {
	return e.dataCh
}

func (e *EM3) pollLoop() {
	defer e.wg.Done()
	ticker := time.NewTicker(e.interval)
	defer ticker.Stop()

	reconnectBackoff := time.Second * 5

	for {
		select {
		case <-e.stopCh:
			return
		case <-ticker.C:
			// Check if we need to reconnect
			e.mu.RLock()
			connected := e.connected
			e.mu.RUnlock()

			if !connected {
				// Attempt reconnection
				log.Printf("shelly: attempting reconnection to %s...", e.addr)
				if err := e.client.Connect(); err != nil {
					e.mu.Lock()
					e.lastError = err.Error()
					e.errorCount++
					e.mu.Unlock()
					log.Printf("shelly: reconnection failed: %v (attempt %d)", err, e.errorCount)
					// Send error status
					data := &core.GridData{Valid: false, Error: "disconnected: " + err.Error()}
					select {
					case e.dataCh <- data:
					default:
					}
					// Back off before next attempt
					select {
					case <-time.After(reconnectBackoff):
					case <-e.stopCh:
						return
					}
					continue
				}
				e.mu.Lock()
				e.connected = true
				e.lastError = ""
				e.errorCount = 0
				e.mu.Unlock()
				log.Printf("shelly: reconnected to %s", e.addr)
			}

			data := e.readAll()
			if !data.Valid {
				e.mu.Lock()
				e.errorCount++
				e.lastError = data.Error
				// After 3 consecutive errors, mark as disconnected
				if e.errorCount >= 3 {
					e.connected = false
					log.Printf("shelly: marking as disconnected after %d errors", e.errorCount)
				}
				e.mu.Unlock()
			} else {
				e.mu.Lock()
				e.errorCount = 0
				e.lastError = ""
				e.mu.Unlock()
			}

			select {
			case e.dataCh <- data:
			default:
			}
		}
	}
}

func (e *EM3) readAll() *core.GridData {
	data := &core.GridData{Valid: true}

	// Read EM1:0 (Phase A) - 20 registers for full block
	phaseARegs, err := e.client.ReadInputRegisters(e.unitID, uint16(em1_0_Base), 20)
	if err != nil {
		log.Printf("shelly: read phase A (EM1:0) error: %v", err)
		return &core.GridData{Valid: false, Error: err.Error()}
	}

	// Read EM1:1 (Phase B) - 20 registers for full block
	phaseBRegs, err := e.client.ReadInputRegisters(e.unitID, uint16(em1_1_Base), 20)
	if err != nil {
		log.Printf("shelly: read phase B (EM1:1) error: %v", err)
		return &core.GridData{Valid: false, Error: err.Error()}
	}

	// Read EM1:2 (Phase C) - 20 registers for full block
	phaseCRegs, err := e.client.ReadInputRegisters(e.unitID, uint16(em1_2_Base), 20)
	if err != nil {
		log.Printf("shelly: read phase C (EM1:2) error: %v", err)
		return &core.GridData{Valid: false, Error: err.Error()}
	}

	// Parse phases - offset 3 is voltage (after timestamp(2)+error(1))
	data.Phases[0] = parsePhaseData(phaseARegs)
	data.Phases[1] = parsePhaseData(phaseBRegs)
	data.Phases[2] = parsePhaseData(phaseCRegs)

	// Select which value to use as the "total" grid power based on configuration
	e.mu.RLock()
	source := e.gridPowerSource
	e.mu.RUnlock()

	switch source {
	case GridPowerPhaseA:
		data.TotalPower = data.Phases[0].ActivePower
		data.TotalCurrent = data.Phases[0].Current
		data.TotalApparent = data.Phases[0].ApparentPower
	case GridPowerPhaseB:
		data.TotalPower = data.Phases[1].ActivePower
		data.TotalCurrent = data.Phases[1].Current
		data.TotalApparent = data.Phases[1].ApparentPower
	case GridPowerPhaseC:
		data.TotalPower = data.Phases[2].ActivePower
		data.TotalCurrent = data.Phases[2].Current
		data.TotalApparent = data.Phases[2].ApparentPower
	case GridPowerPhaseAB:
		// Phase A + Phase B = Site Total Wattage (for PID control)
		data.TotalPower = data.Phases[0].ActivePower + data.Phases[1].ActivePower
		data.TotalCurrent = data.Phases[0].Current + data.Phases[1].Current
		data.TotalApparent = data.Phases[0].ApparentPower + data.Phases[1].ApparentPower
	default: // GridPowerTotal - sum all three phases
		data.TotalPower = data.Phases[0].ActivePower + data.Phases[1].ActivePower + data.Phases[2].ActivePower
		data.TotalCurrent = data.Phases[0].Current + data.Phases[1].Current + data.Phases[2].Current
		data.TotalApparent = data.Phases[0].ApparentPower + data.Phases[1].ApparentPower + data.Phases[2].ApparentPower
	}

	return data
}

// parsePhaseData extracts phase data from a 20-register EM1 block
// Block layout verified empirically: timestamp(2), error(2), voltage(2), current(2), power(2), apparent(2), pf(2), errors(3), freq(2)
func parsePhaseData(regs []uint16) core.PhaseData {
	return core.PhaseData{
		Voltage:       RegistersToFloat32(regs, 4),  // Voltage (V) - offset 4
		Current:       RegistersToFloat32(regs, 6),  // Current (A) - offset 6
		ActivePower:   RegistersToFloat32(regs, 8),  // Power (W) - offset 8
		ApparentPower: RegistersToFloat32(regs, 10), // Apparent (VA) - offset 10
		PowerFactor:   RegistersToFloat32(regs, 12), // Power Factor - offset 12
		Frequency:     60.0,                         // Frequency not reliably at fixed offset, use default
	}
}
