package shelly

import (
	"log"
	"sync"
	"time"

	"github.com/diebietse/invertergui/energymanager/core"
)

// Shelly Pro 3EM Modbus Register Addresses (Input Registers, Function Code 4)
// Official register map from Shelly documentation
// All registers are Float32 (2 registers each, Big-Endian)
const (
	// Phase A (L1) - starting at register 0
	RegPhaseAPower       = 0  // Active Power (W)
	RegPhaseAApparent    = 2  // Apparent Power (VA)
	RegPhaseACurrent     = 4  // Current (A)
	RegPhaseAVoltage     = 6  // Voltage (V)
	RegPhaseAPowerFactor = 8  // Power Factor

	// Phase B (L2) - starting at register 10
	RegPhaseBPower       = 10 // Active Power (W)
	RegPhaseBApparent    = 12 // Apparent Power (VA)
	RegPhaseBCurrent     = 14 // Current (A)
	RegPhaseBVoltage     = 16 // Voltage (V)
	RegPhaseBPowerFactor = 18 // Power Factor

	// Phase C (L3) - starting at register 20
	RegPhaseCPower       = 20 // Active Power (W)
	RegPhaseCApparent    = 22 // Apparent Power (VA)
	RegPhaseCCurrent     = 24 // Current (A)
	RegPhaseCVoltage     = 26 // Voltage (V)
	RegPhaseCPowerFactor = 28 // Power Factor

	// Total values - starting at register 30
	RegTotalPower    = 30 // Total Active Power (W)
	RegTotalApparent = 32 // Total Apparent Power (VA)
	RegTotalCurrent  = 34 // Total Current (A)
)

// EM3 represents a Shelly Pro EM3 energy meter
type EM3 struct {
	client       *ModbusClient
	addr         string
	unitID       byte
	interval     time.Duration
	dataCh       chan *core.GridData
	stopCh       chan struct{}
	wg           sync.WaitGroup
	mu           sync.RWMutex
	connected    bool
	lastError    string
	errorCount   int
	reconnecting bool
}

// NewEM3 creates a new Shelly Pro EM3 driver
func NewEM3(addr string, pollInterval time.Duration) *EM3 {
	if pollInterval == 0 {
		pollInterval = time.Second
	}
	return &EM3{
		client:   NewModbusClient(addr, 5*time.Second),
		addr:     addr,
		unitID:   1, // Shelly default unit ID
		interval: pollInterval,
		dataCh:   make(chan *core.GridData, 8),
		stopCh:   make(chan struct{}),
	}
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

	// Read all data in one request (registers 0-35, 36 registers = 18 floats)
	// This is more efficient than multiple requests
	allRegs, err := e.client.ReadInputRegisters(e.unitID, 0, 36)
	if err != nil {
		log.Printf("shelly: read all error: %v", err)
		return &core.GridData{Valid: false, Error: err.Error()}
	}

	// Parse Phase A (registers 0-9)
	data.Phases[0] = parsePhaseData(allRegs, 0)

	// Parse Phase B (registers 10-19)
	data.Phases[1] = parsePhaseData(allRegs, 10)

	// Parse Phase C (registers 20-29)
	data.Phases[2] = parsePhaseData(allRegs, 20)

	// Parse totals (registers 30-35)
	data.TotalPower = RegistersToFloat32(allRegs, 30)    // Total Active Power
	data.TotalApparent = RegistersToFloat32(allRegs, 32) // Total Apparent Power
	data.TotalCurrent = RegistersToFloat32(allRegs, 34)  // Total Current

	return data
}

func parsePhaseData(regs []uint16, offset int) core.PhaseData {
	return core.PhaseData{
		ActivePower:   RegistersToFloat32(regs, offset+0), // Power (W)
		ApparentPower: RegistersToFloat32(regs, offset+2), // Apparent (VA)
		Current:       RegistersToFloat32(regs, offset+4), // Current (A)
		Voltage:       RegistersToFloat32(regs, offset+6), // Voltage (V)
		PowerFactor:   RegistersToFloat32(regs, offset+8), // Power Factor
		Frequency:     50.0,                               // Not provided, assume 50Hz
	}
}
