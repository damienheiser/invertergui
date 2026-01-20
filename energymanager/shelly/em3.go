package shelly

import (
	"log"
	"sync"
	"time"

	"github.com/diebietse/invertergui/energymanager/core"
)

// Shelly Pro 3EM Modbus Register Addresses (Input Registers)
// These are the actual Modbus addresses (not the 30001+ offset shown in some docs)
// Reference: https://github.com/pipelka/dbus-modbus-shelly
const (
	// Total values
	RegTotalCurrent  = 1011 // float, A
	RegTotalPower    = 1013 // float, W
	RegTotalApparent = 1015 // float, VA

	// Phase A (L1)
	RegPhaseAVoltage     = 1020 // float, V
	RegPhaseACurrent     = 1022 // float, A
	RegPhaseAPower       = 1024 // float, W
	RegPhaseAApparent    = 1026 // float, VA
	RegPhaseAPowerFactor = 1028 // float
	RegPhaseAFrequency   = 1033 // float, Hz

	// Phase B (L2) - offset 20 from Phase A
	RegPhaseBVoltage     = 1040
	RegPhaseBCurrent     = 1042
	RegPhaseBPower       = 1044
	RegPhaseBApparent    = 1046
	RegPhaseBPowerFactor = 1048
	RegPhaseBFrequency   = 1053

	// Phase C (L3) - offset 40 from Phase A
	RegPhaseCVoltage     = 1060
	RegPhaseCCurrent     = 1062
	RegPhaseCPower       = 1064
	RegPhaseCApparent    = 1066
	RegPhaseCPowerFactor = 1068
	RegPhaseCFrequency   = 1073

	// Energy counters
	RegForwardEnergy = 1162 // float, kWh (imported from grid)
	RegReverseEnergy = 1164 // float, kWh (exported to grid)
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

	// Read total values (registers 31011-31016, 6 registers = 3 floats)
	totals, err := e.client.ReadInputRegisters(e.unitID, RegTotalCurrent, 6)
	if err != nil {
		log.Printf("shelly: read totals error: %v", err)
		return &core.GridData{Valid: false, Error: err.Error()}
	}
	data.TotalCurrent = RegistersToFloat32(totals, 0)  // 31011-31012
	data.TotalPower = RegistersToFloat32(totals, 2)    // 31013-31014
	data.TotalApparent = RegistersToFloat32(totals, 4) // 31015-31016

	// Read Phase A (registers 31020-31034)
	phaseA, err := e.client.ReadInputRegisters(e.unitID, RegPhaseAVoltage, 14)
	if err != nil {
		log.Printf("shelly: read phase A error: %v", err)
		data.Valid = false
		data.Error = err.Error()
		return data
	}
	data.Phases[0] = parsePhaseData(phaseA)

	// Read Phase B
	phaseB, err := e.client.ReadInputRegisters(e.unitID, RegPhaseBVoltage, 14)
	if err != nil {
		log.Printf("shelly: read phase B error: %v", err)
		data.Valid = false
		data.Error = err.Error()
		return data
	}
	data.Phases[1] = parsePhaseData(phaseB)

	// Read Phase C
	phaseC, err := e.client.ReadInputRegisters(e.unitID, RegPhaseCVoltage, 14)
	if err != nil {
		log.Printf("shelly: read phase C error: %v", err)
		data.Valid = false
		data.Error = err.Error()
		return data
	}
	data.Phases[2] = parsePhaseData(phaseC)

	return data
}

func parsePhaseData(regs []uint16) core.PhaseData {
	return core.PhaseData{
		Voltage:       RegistersToFloat32(regs, 0),  // offset 0-1
		Current:       RegistersToFloat32(regs, 2),  // offset 2-3
		ActivePower:   RegistersToFloat32(regs, 4),  // offset 4-5
		ApparentPower: RegistersToFloat32(regs, 6),  // offset 6-7
		PowerFactor:   RegistersToFloat32(regs, 8),  // offset 8-9
		Frequency:     RegistersToFloat32(regs, 12), // offset 12-13 (skip 10-11)
	}
}
