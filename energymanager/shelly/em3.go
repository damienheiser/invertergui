package shelly

import (
	"log"
	"sync"
	"time"

	"github.com/diebietse/invertergui/energymanager/core"
)

// Shelly Pro EM3 Modbus Register Addresses (Input Registers)
const (
	// Total values
	RegTotalCurrent  = 31011 // float, A
	RegTotalPower    = 31013 // float, W
	RegTotalApparent = 31015 // float, VA

	// Phase A (offset 0)
	RegPhaseAVoltage       = 31020 // float, V
	RegPhaseACurrent       = 31022 // float, A
	RegPhaseAPower         = 31024 // float, W
	RegPhaseAApparent      = 31026 // float, VA
	RegPhaseAPowerFactor   = 31028 // float
	RegPhaseAFrequency     = 31033 // float, Hz

	// Phase B (offset 20 from Phase A)
	RegPhaseBVoltage       = 31040
	RegPhaseBCurrent       = 31042
	RegPhaseBPower         = 31044
	RegPhaseBApparent      = 31046
	RegPhaseBPowerFactor   = 31048
	RegPhaseBFrequency     = 31053

	// Phase C (offset 40 from Phase A)
	RegPhaseCVoltage       = 31060
	RegPhaseCCurrent       = 31062
	RegPhaseCPower         = 31064
	RegPhaseCApparent      = 31066
	RegPhaseCPowerFactor   = 31068
	RegPhaseCFrequency     = 31073
)

// EM3 represents a Shelly Pro EM3 energy meter
type EM3 struct {
	client   *ModbusClient
	unitID   byte
	interval time.Duration
	dataCh   chan *core.GridData
	stopCh   chan struct{}
	wg       sync.WaitGroup
}

// NewEM3 creates a new Shelly Pro EM3 driver
func NewEM3(addr string, pollInterval time.Duration) *EM3 {
	if pollInterval == 0 {
		pollInterval = time.Second
	}
	return &EM3{
		client:   NewModbusClient(addr, 5*time.Second),
		unitID:   1, // Shelly default unit ID
		interval: pollInterval,
		dataCh:   make(chan *core.GridData, 8),
		stopCh:   make(chan struct{}),
	}
}

// Start begins polling the meter
func (e *EM3) Start() error {
	if err := e.client.Connect(); err != nil {
		return err
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

	for {
		select {
		case <-e.stopCh:
			return
		case <-ticker.C:
			data := e.readAll()
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
