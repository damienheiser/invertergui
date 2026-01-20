// Package vedirect implements the Victron VE.Direct text protocol
// for battery monitors (BMV-700/702/712, SmartShunt, etc.)
package vedirect

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/diebietse/invertergui/energymanager/core"
	"github.com/diebietse/invertergui/energymanager/serial"
)

// BMV represents a Victron Battery Monitor connected via VE.Direct
type BMV struct {
	port     io.ReadWriteCloser
	portPath string
	baudRate int

	mu           sync.RWMutex
	data         core.BatteryData
	raw          map[string]string
	valid        bool
	lastRx       time.Time
	connected    bool
	lastError    string
	reconnecting bool

	stopCh chan struct{}
	wg     sync.WaitGroup
}

// NewBMV creates a new BMV driver instance
func NewBMV(portPath string) *BMV {
	return &BMV{
		portPath: portPath,
		baudRate: 19200, // VE.Direct standard baud rate
		raw:      make(map[string]string),
		stopCh:   make(chan struct{}),
	}
}

// Connected returns true if currently connected
func (b *BMV) Connected() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.connected
}

// LastError returns the last error message
func (b *BMV) LastError() string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.lastError
}

// Connect opens the serial port and starts reading data
func (b *BMV) Connect() error {
	port, err := serial.OpenPort(&serial.Config{
		Name:        b.portPath,
		Baud:        b.baudRate,
		ReadTimeout: time.Second,
	})
	if err != nil {
		b.mu.Lock()
		b.lastError = err.Error()
		b.connected = false
		b.mu.Unlock()
		return fmt.Errorf("failed to open BMV port %s: %w", b.portPath, err)
	}
	b.port = port

	b.mu.Lock()
	b.connected = true
	b.lastError = ""
	b.mu.Unlock()

	b.wg.Add(1)
	go b.readLoop()

	return nil
}

// Close stops the reader and closes the port
func (b *BMV) Close() error {
	close(b.stopCh)
	b.wg.Wait()
	if b.port != nil {
		return b.port.Close()
	}
	return nil
}

// Data returns the current battery data
func (b *BMV) Data() core.BatteryData {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.data
}

// Valid returns true if we have recent valid data
func (b *BMV) Valid() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.valid && time.Since(b.lastRx) < 5*time.Second
}

// RawData returns the raw label/value map for debugging
func (b *BMV) RawData() map[string]string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	result := make(map[string]string)
	for k, v := range b.raw {
		result[k] = v
	}
	return result
}

// readLoop continuously reads VE.Direct text frames
func (b *BMV) readLoop() {
	defer b.wg.Done()

	consecutiveErrors := 0
	const maxErrors = 10

	for {
		select {
		case <-b.stopCh:
			return
		default:
		}

		b.mu.RLock()
		connected := b.connected
		port := b.port
		b.mu.RUnlock()

		if !connected || port == nil {
			// Wait before attempting reconnection
			select {
			case <-time.After(5 * time.Second):
			case <-b.stopCh:
				return
			}

			log.Printf("bmv: attempting reconnection to %s...", b.portPath)
			port, err := serial.OpenPort(&serial.Config{
				Name:        b.portPath,
				Baud:        b.baudRate,
				ReadTimeout: time.Second,
			})
			if err != nil {
				b.mu.Lock()
				b.lastError = err.Error()
				b.mu.Unlock()
				log.Printf("bmv: reconnection failed: %v", err)
				continue
			}

			b.mu.Lock()
			b.port = port
			b.connected = true
			b.lastError = ""
			b.mu.Unlock()
			log.Printf("bmv: reconnected to %s", b.portPath)
			consecutiveErrors = 0
		}

		reader := bufio.NewReader(b.port)
		block := make(map[string]string)
		var checksum byte

		// Read loop for a connected port
		for {
			select {
			case <-b.stopCh:
				return
			default:
			}

			line, err := reader.ReadString('\n')
			if err != nil {
				if err != io.EOF {
					consecutiveErrors++
					if consecutiveErrors >= maxErrors {
						b.mu.Lock()
						b.connected = false
						b.lastError = err.Error()
						b.valid = false
						b.mu.Unlock()
						log.Printf("bmv: disconnected after %d errors: %v", consecutiveErrors, err)
						if b.port != nil {
							b.port.Close()
						}
						break // Break inner loop to attempt reconnection
					}
					time.Sleep(100 * time.Millisecond)
				}
				continue
			}

			consecutiveErrors = 0 // Reset error count on successful read

			// Add all bytes to checksum (including CR LF)
			for i := 0; i < len(line); i++ {
				checksum += line[i]
			}

			// Trim CR LF
			line = strings.TrimRight(line, "\r\n")
			if line == "" {
				continue
			}

			// Split on tab
			parts := strings.SplitN(line, "\t", 2)
			if len(parts) != 2 {
				continue
			}

			label := parts[0]
			value := parts[1]

			if label == "Checksum" {
				// Checksum byte is the value
				if len(value) > 0 {
					checksum += value[0]
				}

				// Valid block if checksum mod 256 == 0
				if checksum == 0 {
					b.processBlock(block)
				}

				// Reset for next block
				block = make(map[string]string)
				checksum = 0
			} else {
				block[label] = value
			}
		}
	}
}

// processBlock converts the raw label/value pairs into structured data
func (b *BMV) processBlock(block map[string]string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	// Copy raw data
	b.raw = block

	// Parse into structured data
	data := core.BatteryData{
		Timestamp: time.Now(),
		Valid:     true,
	}

	// V - Main battery voltage (mV)
	if v, ok := block["V"]; ok {
		if mv, err := strconv.ParseInt(v, 10, 64); err == nil {
			data.VoltageV = float64(mv) / 1000.0
		}
	}

	// VS - Auxiliary (starter) battery voltage (mV)
	if v, ok := block["VS"]; ok {
		if mv, err := strconv.ParseInt(v, 10, 64); err == nil {
			data.AuxVoltageV = float64(mv) / 1000.0
		}
	}

	// I - Current (mA), negative = discharging
	if v, ok := block["I"]; ok {
		if ma, err := strconv.ParseInt(v, 10, 64); err == nil {
			data.CurrentA = float64(ma) / 1000.0
		}
	}

	// P - Instantaneous power (W)
	if v, ok := block["P"]; ok {
		if w, err := strconv.ParseInt(v, 10, 64); err == nil {
			data.PowerW = float64(w)
		}
	}

	// CE - Consumed Ah (mAh)
	if v, ok := block["CE"]; ok {
		if mah, err := strconv.ParseInt(v, 10, 64); err == nil {
			data.ConsumedAh = float64(mah) / 1000.0
		}
	}

	// SOC - State of charge (‰, so 1000 = 100%)
	if v, ok := block["SOC"]; ok {
		if permille, err := strconv.ParseInt(v, 10, 64); err == nil {
			data.SOCPercent = float64(permille) / 10.0
		}
	}

	// TTG - Time to go (minutes), -1 = infinite
	if v, ok := block["TTG"]; ok {
		if mins, err := strconv.ParseInt(v, 10, 64); err == nil {
			data.TimeToGoMin = int(mins)
		}
	}

	// Alarm state
	if v, ok := block["Alarm"]; ok {
		data.AlarmActive = (v == "ON")
	}

	// AR - Alarm reason
	if v, ok := block["AR"]; ok {
		if ar, err := strconv.ParseInt(v, 10, 64); err == nil {
			data.AlarmReason = decodeAlarmReason(int(ar))
		}
	}

	// Temperature (°C * 100, some models)
	if v, ok := block["T"]; ok {
		if t, err := strconv.ParseInt(v, 10, 64); err == nil {
			data.TemperatureC = float64(t)
			data.HasTemperature = true
		}
	}

	// Historical data
	// H1 - Depth of deepest discharge (mAh)
	if v, ok := block["H1"]; ok {
		if mah, err := strconv.ParseInt(v, 10, 64); err == nil {
			data.DeepestDischargeAh = float64(mah) / 1000.0
		}
	}

	// H2 - Depth of last discharge (mAh)
	if v, ok := block["H2"]; ok {
		if mah, err := strconv.ParseInt(v, 10, 64); err == nil {
			data.LastDischargeAh = float64(mah) / 1000.0
		}
	}

	// H3 - Depth of average discharge (mAh)
	if v, ok := block["H3"]; ok {
		if mah, err := strconv.ParseInt(v, 10, 64); err == nil {
			data.AvgDischargeAh = float64(mah) / 1000.0
		}
	}

	// H4 - Number of charge cycles
	if v, ok := block["H4"]; ok {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			data.ChargeCycles = int(n)
		}
	}

	// H5 - Number of full discharges
	if v, ok := block["H5"]; ok {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			data.FullDischarges = int(n)
		}
	}

	// H6 - Cumulative Ah drawn (mAh)
	if v, ok := block["H6"]; ok {
		if mah, err := strconv.ParseInt(v, 10, 64); err == nil {
			data.TotalAhDrawn = float64(mah) / 1000.0
		}
	}

	// H7 - Minimum battery voltage (mV)
	if v, ok := block["H7"]; ok {
		if mv, err := strconv.ParseInt(v, 10, 64); err == nil {
			data.MinVoltageV = float64(mv) / 1000.0
		}
	}

	// H8 - Maximum battery voltage (mV)
	if v, ok := block["H8"]; ok {
		if mv, err := strconv.ParseInt(v, 10, 64); err == nil {
			data.MaxVoltageV = float64(mv) / 1000.0
		}
	}

	// H9 - Seconds since last full charge
	if v, ok := block["H9"]; ok {
		if secs, err := strconv.ParseInt(v, 10, 64); err == nil {
			data.SecondsSinceFullCharge = int(secs)
		}
	}

	// H10 - Number of automatic synchronizations
	if v, ok := block["H10"]; ok {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			data.AutoSyncs = int(n)
		}
	}

	// H11 - Number of low voltage alarms
	if v, ok := block["H11"]; ok {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			data.LowVoltageAlarms = int(n)
		}
	}

	// H12 - Number of high voltage alarms
	if v, ok := block["H12"]; ok {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			data.HighVoltageAlarms = int(n)
		}
	}

	// H17 - Discharged Energy (0.01 kWh)
	if v, ok := block["H17"]; ok {
		if e, err := strconv.ParseInt(v, 10, 64); err == nil {
			data.DischargedKWh = float64(e) / 100.0
		}
	}

	// H18 - Charged Energy (0.01 kWh)
	if v, ok := block["H18"]; ok {
		if e, err := strconv.ParseInt(v, 10, 64); err == nil {
			data.ChargedKWh = float64(e) / 100.0
		}
	}

	// Device info
	// PID - Product ID
	if v, ok := block["PID"]; ok {
		data.ProductID = v
		data.ProductName = decodeProductID(v)
	}

	// FW - Firmware version
	if v, ok := block["FW"]; ok {
		data.FirmwareVersion = v
	}

	// SER# - Serial number
	if v, ok := block["SER#"]; ok {
		data.SerialNumber = v
	}

	// BMV - Model (legacy field)
	if v, ok := block["BMV"]; ok {
		if data.ProductName == "" {
			data.ProductName = "BMV-" + v
		}
	}

	b.data = data
	b.valid = true
	b.lastRx = time.Now()
}

// decodeAlarmReason converts alarm reason bits to human-readable strings
func decodeAlarmReason(ar int) string {
	if ar == 0 {
		return ""
	}
	var reasons []string
	if ar&1 != 0 {
		reasons = append(reasons, "Low Voltage")
	}
	if ar&2 != 0 {
		reasons = append(reasons, "High Voltage")
	}
	if ar&4 != 0 {
		reasons = append(reasons, "Low SOC")
	}
	if ar&8 != 0 {
		reasons = append(reasons, "Low Starter Voltage")
	}
	if ar&16 != 0 {
		reasons = append(reasons, "High Starter Voltage")
	}
	if ar&32 != 0 {
		reasons = append(reasons, "Low Temperature")
	}
	if ar&64 != 0 {
		reasons = append(reasons, "High Temperature")
	}
	if ar&128 != 0 {
		reasons = append(reasons, "Mid Voltage")
	}
	return strings.Join(reasons, ", ")
}

// decodeProductID converts Victron product ID to name
func decodeProductID(pid string) string {
	products := map[string]string{
		"0x203":  "BMV-700",
		"0x204":  "BMV-702",
		"0x205":  "BMV-700H",
		"0xA381": "BMV-712 Smart",
		"0xA382": "BMV-710H Smart",
		"0xA389": "SmartShunt 500A/50mV",
		"0xA38A": "SmartShunt 1000A/50mV",
		"0xA38B": "SmartShunt 2000A/50mV",
	}
	if name, ok := products[pid]; ok {
		return name
	}
	return "Unknown (" + pid + ")"
}
