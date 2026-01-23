package vrm

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/diebietse/invertergui/energymanager/core"
)

// Fixed device instance IDs (consistent, unlike Venus OS)
const (
	DeviceInstanceGrid        = 30 // Grid meter (Shelly Pro 3EM)
	DeviceInstanceMulti       = 0  // MultiPlus II
	DeviceInstanceBattery     = 0  // SmartShunt battery monitor
	DeviceInstanceSystem      = 0  // System summary
	DeviceInstanceTemperature = 0  // Temperature sensors
	DeviceInstancePID         = 100 // Custom PID controller (settings device)
)

// Config for VRM publisher
type Config struct {
	PortalID    string // VRM Portal ID (e.g., b827eb7388f1)
	AccessToken string // VRM access token
	Email       string // VRM account email (optional, token preferred)
	Enabled     bool
}

// Publisher sends data to VRM via MQTT
type Publisher struct {
	config     Config
	client     mqtt.Client
	hub        *core.Hub
	stopCh     chan struct{}
	wg         sync.WaitGroup
	mu         sync.RWMutex
	connected  bool
	lastError  string
	lastPublish time.Time
}

// vrmValue is the JSON payload format for VRM
type vrmValue struct {
	Value interface{} `json:"value"`
}

// New creates a new VRM publisher
func New(hub *core.Hub, config Config) *Publisher {
	return &Publisher{
		config: config,
		hub:    hub,
		stopCh: make(chan struct{}),
	}
}

// Start connects to VRM and begins publishing
func (p *Publisher) Start() error {
	if !p.config.Enabled {
		log.Println("vrm: disabled")
		return nil
	}

	if p.config.PortalID == "" {
		return fmt.Errorf("vrm: portal ID required")
	}
	if p.config.AccessToken == "" {
		return fmt.Errorf("vrm: access token required")
	}

	log.Printf("vrm: starting publisher for portal %s", p.config.PortalID)
	log.Printf("vrm: connecting to mqtt.victronenergy.com:8883...")

	opts := mqtt.NewClientOptions()
	opts.AddBroker("ssl://mqtt.victronenergy.com:8883")
	opts.SetClientID(fmt.Sprintf("energymanager_%s", p.config.PortalID))

	// Authentication: email as username, "Token <token>" as password
	if p.config.Email != "" {
		opts.SetUsername(p.config.Email)
	} else {
		opts.SetUsername(p.config.PortalID)
	}
	opts.SetPassword(fmt.Sprintf("Token %s", p.config.AccessToken))

	// TLS config - skip verification for embedded devices without CA certs
	opts.SetTLSConfig(&tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true, // OpenWRT may not have CA certificates
	})

	opts.SetAutoReconnect(true)
	opts.SetConnectRetry(false) // Don't retry initial connection (causes DNS spam)
	opts.SetConnectRetryInterval(30 * time.Second)
	opts.SetKeepAlive(30 * time.Second)

	opts.SetOnConnectHandler(func(c mqtt.Client) {
		p.mu.Lock()
		p.connected = true
		p.lastError = ""
		p.mu.Unlock()
		log.Printf("vrm: connected to mqtt.victronenergy.com (portal=%s)", p.config.PortalID)
	})

	opts.SetConnectionLostHandler(func(c mqtt.Client, err error) {
		p.mu.Lock()
		p.connected = false
		p.lastError = err.Error()
		p.mu.Unlock()
		log.Printf("vrm: connection lost: %v", err)
	})

	p.client = mqtt.NewClient(opts)

	token := p.client.Connect()
	if token.WaitTimeout(30 * time.Second) {
		if token.Error() != nil {
			return fmt.Errorf("vrm: connect failed: %w", token.Error())
		}
	} else {
		return fmt.Errorf("vrm: connect timeout")
	}

	log.Printf("vrm: connected successfully, starting publish loops")
	p.wg.Add(2)
	go p.publishLoop()
	go p.keepaliveLoop()

	return nil
}

// Stop disconnects from VRM
func (p *Publisher) Stop() {
	close(p.stopCh)
	p.wg.Wait()
	if p.client != nil && p.client.IsConnected() {
		p.client.Disconnect(1000)
	}
	log.Println("vrm: stopped")
}

// Connected returns connection status
func (p *Publisher) Connected() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.connected
}

// LastError returns the last error message
func (p *Publisher) LastError() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.lastError
}

func (p *Publisher) publishLoop() {
	defer p.wg.Done()

	sub := p.hub.Subscribe()
	defer sub.Unsubscribe()

	for {
		select {
		case <-p.stopCh:
			return
		case info := <-sub.C():
			if info != nil {
				p.publishAll(info)
			}
		}
	}
}

func (p *Publisher) keepaliveLoop() {
	defer p.wg.Done()

	ticker := time.NewTicker(50 * time.Second)
	defer ticker.Stop()

	log.Println("vrm: keepalive loop started (50s interval)")

	for {
		select {
		case <-p.stopCh:
			log.Println("vrm: keepalive loop stopped")
			return
		case <-ticker.C:
			if p.client != nil && p.client.IsConnected() {
				topic := fmt.Sprintf("R/%s/keepalive", p.config.PortalID)
				p.client.Publish(topic, 0, false, "")
				log.Printf("vrm: keepalive sent to %s", topic)
			} else {
				log.Println("vrm: skipping keepalive - not connected")
			}
		}
	}
}

func (p *Publisher) publishAll(info *core.EnergyInfo) {
	if !p.client.IsConnected() {
		log.Println("vrm: skipping publish - not connected")
		return
	}

	p.mu.Lock()
	p.lastPublish = time.Now()
	p.mu.Unlock()

	log.Printf("vrm: publishing all data (grid=%v, inverter=%v, battery=%v)",
		info.Grid.Valid, info.Inverter.Valid, info.Battery.Valid)

	// Publish grid meter data (Shelly Pro 3EM)
	p.publishGrid(info)

	// Publish battery data (SmartShunt)
	p.publishBattery(info)

	// Publish inverter data (MultiPlus II)
	p.publishMulti(info)

	// Publish system summary
	p.publishSystem(info)

	// Publish PID controller status (custom)
	p.publishPID(info)

	// Publish OpenWRT system stats
	p.publishOpenWRT()
}

func (p *Publisher) publish(service string, instance int, path string, value interface{}) {
	topic := fmt.Sprintf("N/%s/%s/%d%s", p.config.PortalID, service, instance, path)
	payload, _ := json.Marshal(vrmValue{Value: value})
	p.client.Publish(topic, 0, false, payload)
}

func (p *Publisher) publishGrid(info *core.EnergyInfo) {
	if !info.Grid.Valid {
		log.Println("vrm: skipping grid publish - data invalid")
		return
	}

	inst := DeviceInstanceGrid

	log.Printf("vrm: grid power=%.1fW (L1=%.1fW L2=%.1fW L3=%.1fW)",
		info.Grid.TotalPower,
		info.Grid.Phases[0].ActivePower,
		info.Grid.Phases[1].ActivePower,
		info.Grid.Phases[2].ActivePower)

	// Total power
	p.publish("grid", inst, "/Ac/Power", info.Grid.TotalPower)
	p.publish("grid", inst, "/Ac/Current", info.Grid.TotalCurrent)

	// Phase L1
	p.publish("grid", inst, "/Ac/L1/Voltage", info.Grid.Phases[0].Voltage)
	p.publish("grid", inst, "/Ac/L1/Current", info.Grid.Phases[0].Current)
	p.publish("grid", inst, "/Ac/L1/Power", info.Grid.Phases[0].ActivePower)
	p.publish("grid", inst, "/Ac/L1/PowerFactor", info.Grid.Phases[0].PowerFactor)
	p.publish("grid", inst, "/Ac/L1/Frequency", info.Grid.Phases[0].Frequency)

	// Phase L2
	p.publish("grid", inst, "/Ac/L2/Voltage", info.Grid.Phases[1].Voltage)
	p.publish("grid", inst, "/Ac/L2/Current", info.Grid.Phases[1].Current)
	p.publish("grid", inst, "/Ac/L2/Power", info.Grid.Phases[1].ActivePower)
	p.publish("grid", inst, "/Ac/L2/PowerFactor", info.Grid.Phases[1].PowerFactor)

	// Phase L3
	p.publish("grid", inst, "/Ac/L3/Voltage", info.Grid.Phases[2].Voltage)
	p.publish("grid", inst, "/Ac/L3/Current", info.Grid.Phases[2].Current)
	p.publish("grid", inst, "/Ac/L3/Power", info.Grid.Phases[2].ActivePower)
	p.publish("grid", inst, "/Ac/L3/PowerFactor", info.Grid.Phases[2].PowerFactor)

	// Device info
	p.publish("grid", inst, "/ProductName", "Shelly Pro 3EM")
	p.publish("grid", inst, "/CustomName", "Grid Meter")
	p.publish("grid", inst, "/Connected", 1)
}

func (p *Publisher) publishBattery(info *core.EnergyInfo) {
	inst := DeviceInstanceBattery

	// Use SmartShunt data if available, otherwise inverter battery data
	if info.Battery.Valid {
		log.Printf("vrm: battery (SmartShunt) V=%.2fV I=%.2fA SOC=%.1f%%",
			info.Battery.VoltageV, info.Battery.CurrentA, info.Battery.SOCPercent)
		p.publish("battery", inst, "/Dc/0/Voltage", info.Battery.VoltageV)
		p.publish("battery", inst, "/Dc/0/Current", info.Battery.CurrentA)
		p.publish("battery", inst, "/Dc/0/Power", info.Battery.PowerW)
		p.publish("battery", inst, "/Soc", info.Battery.SOCPercent)
		p.publish("battery", inst, "/ConsumedAmphours", info.Battery.ConsumedAh)
		p.publish("battery", inst, "/TimeToGo", info.Battery.TimeToGoMin*60) // VRM expects seconds

		if info.Battery.HasTemperature {
			p.publish("battery", inst, "/Dc/0/Temperature", info.Battery.TemperatureC)
		}

		// History
		p.publish("battery", inst, "/History/ChargedEnergy", info.Battery.ChargedKWh)
		p.publish("battery", inst, "/History/DischargedEnergy", info.Battery.DischargedKWh)
		p.publish("battery", inst, "/History/ChargeCycles", info.Battery.ChargeCycles)
		p.publish("battery", inst, "/History/DeepestDischarge", info.Battery.DeepestDischargeAh)
		p.publish("battery", inst, "/History/LastDischarge", info.Battery.LastDischargeAh)
		p.publish("battery", inst, "/History/AverageDischarge", info.Battery.AvgDischargeAh)
		p.publish("battery", inst, "/History/TotalAhDrawn", info.Battery.TotalAhDrawn)
		p.publish("battery", inst, "/History/MinimumVoltage", info.Battery.MinVoltageV)
		p.publish("battery", inst, "/History/MaximumVoltage", info.Battery.MaxVoltageV)
		p.publish("battery", inst, "/History/TimeSinceLastFullCharge", info.Battery.SecondsSinceFullCharge)
		p.publish("battery", inst, "/History/AutomaticSyncs", info.Battery.AutoSyncs)
		p.publish("battery", inst, "/History/LowVoltageAlarms", info.Battery.LowVoltageAlarms)
		p.publish("battery", inst, "/History/HighVoltageAlarms", info.Battery.HighVoltageAlarms)

		// Alarms
		if info.Battery.AlarmActive {
			p.publish("battery", inst, "/Alarms/Alarm", 1)
		} else {
			p.publish("battery", inst, "/Alarms/Alarm", 0)
		}

		// Device info
		p.publish("battery", inst, "/ProductName", info.Battery.ProductName)
		p.publish("battery", inst, "/FirmwareVersion", info.Battery.FirmwareVersion)
		p.publish("battery", inst, "/Connected", 1)
	} else if info.Inverter.Valid {
		// Fallback to inverter battery data
		log.Printf("vrm: battery (from inverter) V=%.2fV I=%.2fA SOC=%.1f%%",
			info.Inverter.BatVoltage, info.Inverter.BatCurrent, info.Inverter.BatSOC)
		p.publish("battery", inst, "/Dc/0/Voltage", info.Inverter.BatVoltage)
		p.publish("battery", inst, "/Dc/0/Current", info.Inverter.BatCurrent)
		p.publish("battery", inst, "/Dc/0/Power", info.Inverter.BatPower)
		p.publish("battery", inst, "/Soc", info.Inverter.BatSOC)
		p.publish("battery", inst, "/ProductName", "MultiPlus II Battery")
		p.publish("battery", inst, "/Connected", 1)
	}
}

func (p *Publisher) publishMulti(info *core.EnergyInfo) {
	if !info.Inverter.Valid {
		log.Println("vrm: skipping multi publish - data invalid")
		return
	}

	inst := DeviceInstanceMulti

	log.Printf("vrm: multi state=%s ACin=%.1fV/%.1fA ACout=%.1fV/%.1fA Bat=%.1fV/%.1fA",
		info.Inverter.State,
		info.Inverter.ACInVoltage, info.Inverter.ACInCurrent,
		info.Inverter.ACOutVoltage, info.Inverter.ACOutCurrent,
		info.Inverter.BatVoltage, info.Inverter.BatCurrent)

	// AC Input (Grid/Mains)
	p.publish("vebus", inst, "/Ac/ActiveIn/L1/V", info.Inverter.ACInVoltage)
	p.publish("vebus", inst, "/Ac/ActiveIn/L1/I", info.Inverter.ACInCurrent)
	p.publish("vebus", inst, "/Ac/ActiveIn/L1/F", info.Inverter.ACInFreq)
	p.publish("vebus", inst, "/Ac/ActiveIn/L1/P", info.Inverter.ACInVoltage*info.Inverter.ACInCurrent)

	// AC Output (Inverter/Loads)
	p.publish("vebus", inst, "/Ac/Out/L1/V", info.Inverter.ACOutVoltage)
	p.publish("vebus", inst, "/Ac/Out/L1/I", info.Inverter.ACOutCurrent)
	p.publish("vebus", inst, "/Ac/Out/L1/F", info.Inverter.ACOutFreq)
	p.publish("vebus", inst, "/Ac/Out/L1/P", info.Inverter.ACOutVoltage*info.Inverter.ACOutCurrent)

	// DC (Battery)
	p.publish("vebus", inst, "/Dc/0/Voltage", info.Inverter.BatVoltage)
	p.publish("vebus", inst, "/Dc/0/Current", info.Inverter.BatCurrent)
	p.publish("vebus", inst, "/Dc/0/Power", info.Inverter.BatPower)

	// State
	state := p.inverterStateToVRM(info.Inverter.State)
	p.publish("vebus", inst, "/State", state)
	p.publish("vebus", inst, "/VebusChargeState", state)

	// SOC
	p.publish("vebus", inst, "/Soc", info.Inverter.BatSOC)

	// Device info
	p.publish("vebus", inst, "/ProductName", "MultiPlus-II 48/5000")
	p.publish("vebus", inst, "/CustomName", "Inverter")
	p.publish("vebus", inst, "/Connected", 1)
}

func (p *Publisher) publishSystem(info *core.EnergyInfo) {
	inst := DeviceInstanceSystem

	// Battery summary
	if info.Battery.Valid {
		p.publish("system", inst, "/Dc/Battery/Voltage", info.Battery.VoltageV)
		p.publish("system", inst, "/Dc/Battery/Current", info.Battery.CurrentA)
		p.publish("system", inst, "/Dc/Battery/Power", info.Battery.PowerW)
		p.publish("system", inst, "/Dc/Battery/Soc", info.Battery.SOCPercent)
		p.publish("system", inst, "/Dc/Battery/TimeToGo", info.Battery.TimeToGoMin*60)
	} else if info.Inverter.Valid {
		p.publish("system", inst, "/Dc/Battery/Voltage", info.Inverter.BatVoltage)
		p.publish("system", inst, "/Dc/Battery/Current", info.Inverter.BatCurrent)
		p.publish("system", inst, "/Dc/Battery/Power", info.Inverter.BatPower)
		p.publish("system", inst, "/Dc/Battery/Soc", info.Inverter.BatSOC)
	}

	// Grid power
	if info.Grid.Valid {
		p.publish("system", inst, "/Ac/Grid/L1/Power", info.Grid.TotalPower)
		p.publish("system", inst, "/Ac/Consumption/L1/Power", info.Grid.TotalPower+info.Inverter.BatPower)
	}

	// System state
	p.publish("system", inst, "/SystemState/State", p.inverterStateToVRM(info.Inverter.State))
}

func (p *Publisher) publishPID(info *core.EnergyInfo) {
	inst := DeviceInstancePID

	// PID controller data as a "settings" device
	p.publish("settings", inst, "/Settings/CGwacs/AcPowerSetPoint", info.PID.Setpoint)
	p.publish("settings", inst, "/Settings/CGwacs/BatteryLife/State", 0) // Disabled
	p.publish("settings", inst, "/Settings/CGwacs/MaxDischargePower", info.Config.MaxPower)
	p.publish("settings", inst, "/Settings/CGwacs/MaxChargePower", info.Config.MaxPower)

	// Custom PID values (using a pvinverter service as a hack to show in VRM)
	p.publish("pvinverter", inst, "/Ac/Power", info.PID.Output)
	p.publish("pvinverter", inst, "/Ac/L1/Power", info.PID.Output)
	p.publish("pvinverter", inst, "/ProductName", "PID Controller")
	p.publish("pvinverter", inst, "/CustomName", fmt.Sprintf("Grid=%.0fW Err=%.0fW Out=%.0fW", info.PID.GridPower, info.PID.Error, info.PID.Output))
	p.publish("pvinverter", inst, "/Position", 1) // AC Output
	p.publish("pvinverter", inst, "/Connected", 1)

	// Mode info
	p.publish("settings", inst, "/Settings/SystemSetup/SystemName", fmt.Sprintf("Mode: %s", info.Mode.Current))
}

func (p *Publisher) publishOpenWRT() {
	inst := DeviceInstanceTemperature

	// CPU temperature (if available)
	if temp, err := p.readCPUTemp(); err == nil {
		p.publish("temperature", inst, "/Temperature", temp)
		p.publish("temperature", inst, "/ProductName", "OpenWRT CPU")
		p.publish("temperature", inst, "/CustomName", "Router Temperature")
		p.publish("temperature", inst, "/Connected", 1)
	}

	// System load (as a custom value)
	if load, err := p.readLoadAvg(); err == nil {
		p.publish("temperature", inst+1, "/Temperature", load*10) // Scale for visibility
		p.publish("temperature", inst+1, "/ProductName", "System Load")
		p.publish("temperature", inst+1, "/CustomName", fmt.Sprintf("Load: %.2f", load))
		p.publish("temperature", inst+1, "/Connected", 1)
	}
}

func (p *Publisher) readCPUTemp() (float64, error) {
	// Try common thermal zone paths
	paths := []string{
		"/sys/class/thermal/thermal_zone0/temp",
		"/sys/devices/virtual/thermal/thermal_zone0/temp",
	}

	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err == nil {
			var temp int
			if _, err := fmt.Sscanf(string(data), "%d", &temp); err == nil {
				return float64(temp) / 1000.0, nil // Convert millidegrees to degrees
			}
		}
	}
	return 0, fmt.Errorf("no thermal zone found")
}

func (p *Publisher) readLoadAvg() (float64, error) {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0, err
	}
	var load1, load5, load15 float64
	if _, err := fmt.Sscanf(string(data), "%f %f %f", &load1, &load5, &load15); err == nil {
		return load1, nil
	}
	return 0, fmt.Errorf("failed to parse loadavg")
}

func (p *Publisher) inverterStateToVRM(state string) int {
	// VRM state codes
	switch state {
	case "off":
		return 0
	case "low_bat", "low_power":
		return 1
	case "fault":
		return 2
	case "bulk":
		return 3
	case "absorption":
		return 4
	case "float":
		return 5
	case "storage":
		return 6
	case "equalize":
		return 7
	case "passthru", "bypass":
		return 8
	case "inverting":
		return 9
	case "assisting":
		return 10
	case "charge":
		return 3 // Bulk charging
	default:
		return 0
	}
}
