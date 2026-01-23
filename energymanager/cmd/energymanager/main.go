package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/diebietse/invertergui/energymanager/core"
	"github.com/diebietse/invertergui/energymanager/discovery"
	"github.com/diebietse/invertergui/energymanager/influxdb"
	"github.com/diebietse/invertergui/energymanager/modes"
	"github.com/diebietse/invertergui/energymanager/mqtt"
	"github.com/diebietse/invertergui/energymanager/pid"
	"github.com/diebietse/invertergui/energymanager/prometheus"
	"github.com/diebietse/invertergui/energymanager/rrd"
	"github.com/diebietse/invertergui/energymanager/serial"
	"github.com/diebietse/invertergui/energymanager/shelly"
	"github.com/diebietse/invertergui/energymanager/usbdetect"
	"github.com/diebietse/invertergui/energymanager/vebus"
	"github.com/diebietse/invertergui/energymanager/vedirect"
	"github.com/diebietse/invertergui/energymanager/webui"
)

var (
	// Server - port 8081 to avoid conflict with GL.iNet admin panel on 8080
	listenAddr = flag.String("listen", ":8081", "HTTP listen address")

	// Shelly Pro EM3
	shellyAddr         = flag.String("shelly.addr", "", "Shelly Pro EM3 address (host:port), auto-discover if empty")
	shellyInterval     = flag.Duration("shelly.interval", time.Second, "Shelly poll interval")
	shellyAutoConfig   = flag.Bool("shelly.autoconfig", true, "Auto-discover and configure Shelly")
	shellyGridSource   = flag.String("shelly.gridsource", "phase_c", "Grid power source: total, phase_a, phase_b, phase_c")

	// Victron MK2/MK3 (VE.Bus inverter control)
	victronPort     = flag.String("victron.port", "/dev/ttyUSB0", "Victron VE.Bus serial port (MK2/MK3)")
	victronEnabled  = flag.Bool("victron.enabled", true, "Enable Victron inverter control")
	victronMaxPower = flag.Float64("victron.maxpower", 5000, "Max inverter power (W)")

	// Victron BMV/SmartShunt (VE.Direct battery monitor)
	bmvPort    = flag.String("bmv.port", "/dev/ttyUSB1", "Victron BMV VE.Direct serial port")
	bmvEnabled = flag.Bool("bmv.enabled", true, "Enable Victron BMV battery monitor")

	// Battery
	batteryCapacity = flag.Float64("battery.capacity", 10.0, "Battery capacity in kWh")

	// Location (for sunrise calculation)
	latitude  = flag.Float64("latitude", 40.7128, "Latitude for sunrise calculation")
	longitude = flag.Float64("longitude", -74.0060, "Longitude for sunrise calculation")

	// PID Controller
	pidEnabled  = flag.Bool("pid.enabled", true, "Enable PID controller")
	pidKp       = flag.Float64("pid.kp", 0.8, "PID proportional gain")
	pidKi       = flag.Float64("pid.ki", 0.05, "PID integral gain")
	pidKd       = flag.Float64("pid.kd", 0.1, "PID derivative gain")
	pidSetpoint = flag.Float64("pid.setpoint", 0, "PID setpoint (target grid power)")

	// Operating mode
	defaultMode = flag.String("mode", "pid", "Default operating mode (manual, pid, tou, solar, battery_save)")

	// MQTT
	mqttEnabled  = flag.Bool("mqtt.enabled", false, "Enable MQTT publishing")
	mqttBroker   = flag.String("mqtt.broker", "tcp://localhost:1883", "MQTT broker address")
	mqttTopic    = flag.String("mqtt.topic", "energy/status", "MQTT topic")
	mqttClientID = flag.String("mqtt.clientid", "energymanager", "MQTT client ID")
	mqttUsername = flag.String("mqtt.username", "", "MQTT username")
	mqttPassword = flag.String("mqtt.password", "", "MQTT password")

	// InfluxDB
	influxEnabled  = flag.Bool("influx.enabled", false, "Enable InfluxDB time series export")
	influxURL      = flag.String("influx.url", "http://localhost:8086", "InfluxDB server URL")
	influxDatabase = flag.String("influx.database", "energy", "InfluxDB database name (1.x) or bucket (2.x)")
	influxUsername = flag.String("influx.username", "", "InfluxDB username (1.x)")
	influxPassword = flag.String("influx.password", "", "InfluxDB password (1.x)")
	influxToken    = flag.String("influx.token", "", "InfluxDB token (2.x)")
	influxOrg      = flag.String("influx.org", "", "InfluxDB organization (2.x)")
	influxInterval = flag.Duration("influx.interval", 5*time.Minute, "InfluxDB batch flush interval")

	// Auth (for web UI config/control pages)
	authUsername = flag.String("auth.username", "", "Admin username (default: admin)")
	authPassword = flag.String("auth.password", "", "Admin password (default: energy)")
)

func main() {
	flag.Parse()
	log.SetFlags(log.Ltime | log.Lshortfile)
	log.Println("Starting Energy Manager")
	log.Printf("  Listen address: %s", *listenAddr)
	log.Printf("  Shelly address: %s (autoconfig=%v)", *shellyAddr, *shellyAutoConfig)
	log.Printf("  Victron enabled=%v port=%s maxpower=%.0f", *victronEnabled, *victronPort, *victronMaxPower)
	log.Printf("  BMV enabled=%v port=%s", *bmvEnabled, *bmvPort)

	// Create hub for broadcasting data
	hub := core.NewHub()

	// Create in-memory RRD for historical data
	rrdStore := rrd.New(rrd.DefaultConfig())
	log.Println("RRD storage initialized (5-min/24h, 1h/30d archives)")

	// Create mode controller early
	modeCtrl := modes.New(modes.Config{
		MaxPower:        *victronMaxPower,
		BatteryCapacity: *batteryCapacity,
		Latitude:        *latitude,
		Longitude:       *longitude,
	})
	modeCtrl.SetMode(core.OperatingMode(*defaultMode))

	// Create web UI config (will be updated as we discover devices)
	webConfig := &webui.Config{
		ShellyAddr:      *shellyAddr,
		VictronPort:     *victronPort,
		BMVPort:         *bmvPort,
		MaxPower:        *victronMaxPower,
		BatteryCapacity: *batteryCapacity,
		Latitude:        *latitude,
		Longitude:       *longitude,
		PIDKp:           *pidKp,
		PIDKi:           *pidKi,
		PIDKd:           *pidKd,
		PIDSetpoint:     *pidSetpoint,
		SolarConfig: core.SolarConfig{
			MinSOC: 20,
			MaxSOC: 100,
		},
		BatterySaveConfig: core.BatterySaveConfig{
			MinSOC:        20,
			TargetSOC:     30,
			SunriseOffset: 30,
		},
		Username: *authUsername,
		Password: *authPassword,
		// MQTT config
		MQTTEnabled:  *mqttEnabled,
		MQTTBroker:   *mqttBroker,
		MQTTTopic:    *mqttTopic,
		MQTTClientID: *mqttClientID,
		MQTTUsername: *mqttUsername,
		MQTTPassword: *mqttPassword,
		// InfluxDB config
		InfluxEnabled:  *influxEnabled,
		InfluxURL:      *influxURL,
		InfluxDatabase: *influxDatabase,
		InfluxUsername: *influxUsername,
		InfluxPassword: *influxPassword,
		InfluxToken:    *influxToken,
		InfluxOrg:      *influxOrg,
		InfluxInterval: *influxInterval,
	}

	// Create web handler and register routes FIRST
	// This ensures the UI is available during hardware initialization
	webHandler := webui.New(hub, modeCtrl, webConfig)
	mux := http.NewServeMux()

	// Health check endpoint (always available)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})

	// Register all web routes immediately
	webHandler.RegisterRoutes(mux)

	// Prometheus metrics
	promHandler := prometheus.New(hub)
	mux.Handle("/metrics", promHandler)

	// Start HTTP server in background immediately
	go func() {
		log.Printf("HTTP server starting on %s", *listenAddr)
		if err := http.ListenAndServe(*listenAddr, mux); err != nil {
			log.Printf("HTTP server error: %v", err)
		}
	}()
	log.Println("HTTP server started - UI available during initialization")

	// ============================================
	// Hardware Initialization (non-blocking to UI)
	// ============================================

	// Stage 1: Shelly Discovery
	webHandler.SetInitStatus("shelly", "Discovering Shelly Pro EM3...", false, "")
	shellyAddress := *shellyAddr
	if shellyAddress == "" && *shellyAutoConfig {
		log.Println("Auto-discovering Shelly Pro EM3...")
		shellyAddress = discoverShelly()
	}
	if shellyAddress == "" {
		log.Println("Warning: No Shelly address configured. Use -shelly.addr or enable auto-discovery")
	}

	// Stage 2: Connect to Shelly
	var em3 *shelly.EM3
	if shellyAddress != "" {
		webHandler.SetInitStatus("shelly", "Connecting to Shelly at "+shellyAddress+"...", false, "")
		gridSource := shelly.GridPowerSource(*shellyGridSource)
		em3 = shelly.NewEM3(shellyAddress, *shellyInterval, gridSource)
		if err := em3.Start(); err != nil {
			log.Printf("Warning: Failed to start Shelly EM3: %v", err)
			webHandler.SetInitStatus("shelly", "Shelly connection failed: "+err.Error(), false, err.Error())
			em3 = nil
		} else {
			log.Printf("Shelly Pro EM3 polling at %s (grid source=%s)", shellyAddress, *shellyGridSource)
			webConfig.ShellyAddr = shellyAddress
		}
	}

	// ============================================
	// Stage 3: VE.Bus (inverter) Init - uses USB device detection
	// ============================================
	webHandler.SetInitStatus("vebus", "Detecting VE.Bus devices...", false, "")
	log.Println("Initializing Victron VE.Bus (inverter)...")

	// Detect all USB serial devices
	allDevices := usbdetect.FindAllDevices()
	log.Printf("[USB] Found %d USB serial devices:", len(allDevices))
	for _, dev := range allDevices {
		log.Printf("[USB]   %s: %s (%s) type=%d", dev.Port, dev.Product, dev.Manufacturer, dev.Type)
	}

	var vbus *vebus.VEBus
	var vbusPort string

	if *victronEnabled {
		// Build list of VE.Bus ports (MK2/MK3 interfaces)
		var portsToTry []string

		// First, try ports detected as VE.Bus devices
		vebusDevices := usbdetect.FindDevicesByType(usbdetect.DeviceVEBus)
		for _, dev := range vebusDevices {
			log.Printf("[VE.Bus] Detected MK2/MK3 interface: %s (%s)", dev.Port, dev.Product)
			portsToTry = append(portsToTry, dev.Port)
		}

		// If configured port not in detected list, add it (fallback)
		configuredInList := false
		for _, p := range portsToTry {
			if p == *victronPort {
				configuredInList = true
				break
			}
		}
		if !configuredInList && *victronPort != "" {
			log.Printf("[VE.Bus] Adding configured port %s (not detected as MK2/MK3)", *victronPort)
			portsToTry = append(portsToTry, *victronPort)
		}

		if len(portsToTry) == 0 {
			log.Println("[VE.Bus] No MK2/MK3 interfaces detected")
			webHandler.SetInitStatus("vebus", "No MK2/MK3 detected", false, "no device")
		} else {
			log.Printf("[VE.Bus] Will try ports: %v", portsToTry)
		}

	portLoop:
		for i, port := range portsToTry {
			log.Printf("[VE.Bus] Trying port %d/%d: %s at 2400 baud", i+1, len(portsToTry), port)
			webHandler.SetInitStatus("vebus", "Connecting to "+port+"...", false, "")

			serialPort, err := serial.OpenPort(&serial.Config{
				Name:        port,
				Baud:        2400,
				ReadTimeout: 500 * time.Millisecond,
			})
			if err != nil {
				log.Printf("[VE.Bus] Port %s unavailable: %v", port, err)
				continue portLoop
			}
			log.Printf("[VE.Bus] Opened port %s successfully", port)

			// Try to connect with timeout
			tmpVbus := vebus.New(serialPort, *victronMaxPower)

			connectDone := make(chan error, 1)
			go func() {
				connectDone <- tmpVbus.Connect()
			}()

			select {
			case err := <-connectDone:
				if err != nil {
					log.Printf("[VE.Bus] Connect failed on %s: %v", port, err)
					serialPort.Close()
					continue portLoop
				}
				log.Printf("[VE.Bus] Connected via %s (max %.0fW)", port, *victronMaxPower)
				vbus = tmpVbus
				vbusPort = port
				webConfig.VictronPort = port
				break portLoop
			case <-time.After(10 * time.Second):
				log.Printf("[VE.Bus] Connect timeout on %s, closing port...", port)
				if setter, ok := serialPort.(interface{ SetReadDeadline(time.Time) error }); ok {
					setter.SetReadDeadline(time.Now())
				}
				closeDone := make(chan struct{})
				go func() {
					serialPort.Close()
					close(closeDone)
				}()
				select {
				case <-closeDone:
					log.Printf("[VE.Bus] Port %s closed", port)
				case <-time.After(2 * time.Second):
					log.Printf("[VE.Bus] Port %s close timeout (ignoring)", port)
				}
				continue portLoop
			}
		}

		if vbus == nil {
			log.Println("[VE.Bus] No device found on any port")
			webHandler.SetInitStatus("vebus", "No VE.Bus device found", false, "no device")
		}
	}

	// ============================================
	// Stage 4: BMV (battery monitor) Init - uses USB device detection
	// ============================================
	webHandler.SetInitStatus("bmv", "Detecting VE.Direct devices...", false, "")
	log.Println("Initializing Victron BMV (battery monitor)...")

	var bmv *vedirect.BMV

	if *bmvEnabled {
		// Build list of VE.Direct ports
		var bmvPortsToTry []string

		// First, try ports detected as VE.Direct devices
		vedirectDevices := usbdetect.FindDevicesByType(usbdetect.DeviceVEDirect)
		for _, dev := range vedirectDevices {
			if dev.Port != vbusPort { // Exclude VE.Bus port
				log.Printf("[BMV] Detected VE.Direct cable: %s (%s)", dev.Port, dev.Product)
				bmvPortsToTry = append(bmvPortsToTry, dev.Port)
			}
		}

		// If configured port not in detected list, add it (fallback)
		configuredInList := false
		for _, p := range bmvPortsToTry {
			if p == *bmvPort {
				configuredInList = true
				break
			}
		}
		if !configuredInList && *bmvPort != "" && *bmvPort != vbusPort {
			log.Printf("[BMV] Adding configured port %s (not detected as VE.Direct)", *bmvPort)
			bmvPortsToTry = append(bmvPortsToTry, *bmvPort)
		}

		if vbusPort != "" {
			log.Printf("[BMV] Excluding %s (used by VE.Bus)", vbusPort)
		}

		if len(bmvPortsToTry) == 0 {
			log.Println("[BMV] No VE.Direct cables detected")
			webHandler.SetInitStatus("bmv", "No VE.Direct detected", false, "no device")
		} else {
			log.Printf("[BMV] Will try ports: %v", bmvPortsToTry)
		}

	bmvPortLoop:
		for i, port := range bmvPortsToTry {
			log.Printf("[BMV] Trying port %d/%d: %s", i+1, len(bmvPortsToTry), port)
			webHandler.SetInitStatus("bmv", "Connecting to "+port+"...", false, "")

			tmpBMV := vedirect.NewBMV(port)

			connectDone := make(chan error, 1)
			go func() {
				connectDone <- tmpBMV.Connect()
			}()

			select {
			case err := <-connectDone:
				if err != nil {
					log.Printf("[BMV] Connect failed on %s: %v", port, err)
					continue bmvPortLoop
				}
				log.Printf("[BMV] Connected via %s", port)
				bmv = tmpBMV
				webConfig.BMVPort = port
				break bmvPortLoop
			case <-time.After(5 * time.Second):
				log.Printf("[BMV] Connect timeout on %s", port)
				continue bmvPortLoop
			}
		}

		if bmv == nil {
			log.Println("[BMV] No device found on any port")
			webHandler.SetInitStatus("bmv", "No BMV device found", false, "no device")
		}
	}

	log.Println("Hardware initialization complete")

	if vbus != nil {
		log.Printf("VE.Bus: connected on %s", webConfig.VictronPort)
	} else {
		log.Println("VE.Bus: not connected")
	}
	if bmv != nil {
		log.Printf("BMV: connected on %s", webConfig.BMVPort)
	} else {
		log.Println("BMV: not connected")
	}

	// Stage 5: PID Controller
	webHandler.SetInitStatus("pid", "Initializing PID controller...", false, "")
	var pidCtrl *pid.Controller
	if *pidEnabled {
		pidCtrl = pid.New(pid.Config{
			Kp:              *pidKp,
			Ki:              *pidKi,
			Kd:              *pidKd,
			Setpoint:        *pidSetpoint,
			MinOutput:       -*victronMaxPower,
			MaxOutput:       *victronMaxPower,
			MaxIntegral:     2000,
			Deadband:        20,
			MaxRateOfChange: 500,
		})
		log.Printf("PID controller enabled (Kp=%.2f Ki=%.2f Kd=%.2f)", *pidKp, *pidKi, *pidKd)
	}

	// Start main control loop
	webHandler.SetInitStatus("control", "Starting control loop...", false, "")
	log.Println("Starting control loop...")
	go controlLoop(hub, em3, vbus, bmv, pidCtrl, modeCtrl, webConfig, rrdStore)

	// Service managers for runtime reconfiguration
	svcMgr := &serviceManager{
		hub: hub,
	}

	// Start MQTT if enabled (from flags or saved config)
	if webConfig.MQTTEnabled {
		webHandler.SetInitStatus("mqtt", "Starting MQTT publisher...", false, "")
		if err := svcMgr.startMQTT(webConfig.MQTTBroker, webConfig.MQTTTopic, webConfig.MQTTClientID, webConfig.MQTTUsername, webConfig.MQTTPassword); err != nil {
			log.Printf("Warning: Failed to start MQTT: %v", err)
		}
	}

	// Start InfluxDB writer if enabled (from flags or saved config)
	if webConfig.InfluxEnabled {
		webHandler.SetInitStatus("influxdb", "Starting InfluxDB writer...", false, "")
		if err := svcMgr.startInfluxDB(webConfig.InfluxURL, webConfig.InfluxDatabase, webConfig.InfluxUsername, webConfig.InfluxPassword, webConfig.InfluxToken, webConfig.InfluxOrg, webConfig.InfluxInterval); err != nil {
			log.Printf("Warning: Failed to start InfluxDB writer: %v", err)
		}
	}

	// Set up runtime reconfiguration callbacks
	webHandler.SetCallbacks(&webui.ServiceCallbacks{
		ReconfigureMQTT: func(enabled bool, broker, topic, clientID, username, password string) error {
			svcMgr.stopMQTT()
			if enabled {
				return svcMgr.startMQTT(broker, topic, clientID, username, password)
			}
			return nil
		},
		ReconfigureInfluxDB: func(enabled bool, url, database, username, password, token, org string, interval time.Duration) error {
			svcMgr.stopInfluxDB()
			if enabled {
				return svcMgr.startInfluxDB(url, database, username, password, token, org, interval)
			}
			return nil
		},
	})

	// Mark initialization complete
	webHandler.SetInitStatus("ready", "Energy Manager running", true, "")
	log.Println("Energy Manager initialization complete")

	// Wait for shutdown signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	log.Println("Shutting down...")

	// Stop services
	svcMgr.stopAll()

	if vbus != nil {
		vbus.SetPower(0) // Safe shutdown
		vbus.Close()
	}
	if bmv != nil {
		bmv.Close()
	}
	if em3 != nil {
		em3.Stop()
	}
}

// serviceManager handles runtime service lifecycle
type serviceManager struct {
	hub          *core.Hub
	mqttPub      *mqtt.Publisher
	influxWriter *influxdb.Writer
	mu           sync.Mutex
}

func (sm *serviceManager) startMQTT(broker, topic, clientID, username, password string) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if sm.mqttPub != nil {
		sm.mqttPub.Stop()
	}

	sm.mqttPub = mqtt.New(sm.hub, mqtt.Config{
		Broker:   broker,
		Topic:    topic,
		ClientID: clientID,
		Username: username,
		Password: password,
	})

	if err := sm.mqttPub.Start(); err != nil {
		sm.mqttPub = nil
		return err
	}

	log.Printf("MQTT publishing to %s topic %s", broker, topic)
	return nil
}

func (sm *serviceManager) stopMQTT() {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if sm.mqttPub != nil {
		sm.mqttPub.Stop()
		sm.mqttPub = nil
		log.Println("MQTT publisher stopped")
	}
}

func (sm *serviceManager) startInfluxDB(url, database, username, password, token, org string, interval time.Duration) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if sm.influxWriter != nil {
		sm.influxWriter.Stop()
	}

	if interval == 0 {
		interval = 5 * time.Minute
	}

	sm.influxWriter = influxdb.New(sm.hub, influxdb.Config{
		URL:           url,
		Database:      database,
		Username:      username,
		Password:      password,
		Token:         token,
		Org:           org,
		Bucket:        database,
		BatchInterval: interval,
		MaxBatchSize:  1000,
		Measurement:   "energy_status",
		Tags:          map[string]string{"host": "energymanager"},
		MaxRetries:    3,
		RetryDelay:    5 * time.Second,
	})

	if err := sm.influxWriter.Start(); err != nil {
		sm.influxWriter = nil
		return err
	}

	log.Printf("InfluxDB writer to %s/%s (batch=%v)", url, database, interval)
	return nil
}

func (sm *serviceManager) stopInfluxDB() {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if sm.influxWriter != nil {
		sm.influxWriter.Stop()
		sm.influxWriter = nil
		log.Println("InfluxDB writer stopped")
	}
}

func (sm *serviceManager) stopAll() {
	sm.stopMQTT()
	sm.stopInfluxDB()
}

func discoverShelly() string {
	scanner := discovery.NewScanner()

	// First try local network scan
	devices, err := scanner.ScanLocalNetwork()
	if err != nil {
		log.Printf("Network scan error: %v", err)
		return ""
	}

	em3Devices := scanner.FindEM3Devices(devices)
	if len(em3Devices) == 0 {
		log.Println("No Shelly Pro EM3 devices found")
		return ""
	}

	// Use first EM3 found
	device := em3Devices[0]
	log.Printf("Found Shelly %s at %s", device.Model, device.IP)

	// Enable Modbus if not already
	if !device.Modbus {
		log.Printf("Enabling Modbus on %s...", device.IP)
		if err := scanner.EnableModbus(device.IP); err != nil {
			log.Printf("Warning: Failed to enable Modbus: %v", err)
		}
	}

	return device.IP + ":502"
}

func controlLoop(hub *core.Hub, em3 *shelly.EM3, vbus *vebus.VEBus, bmv *vedirect.BMV, pidCtrl *pid.Controller, modeCtrl *modes.Controller, webConfig *webui.Config, rrdStore *rrd.RRD) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for range ticker.C {
		info := &core.EnergyInfo{
			Timestamp: time.Now(),
			Valid:     true,
			Config: core.ConfigData{
				ShellyAddr:       webConfig.ShellyAddr,
				VictronPort:      webConfig.VictronPort,
				BMVPort:          webConfig.BMVPort,
				MaxPower:         webConfig.MaxPower,
				BatteryCapacity:  webConfig.BatteryCapacity,
				ShellyConnected:  em3 != nil,
				VictronConnected: vbus != nil,
				BMVConnected:     bmv != nil && bmv.Valid(),
			},
		}

		// Read grid data from Shelly
		if em3 != nil {
			// Check connection status
			info.Config.ShellyConnected = em3.Connected()
			if !info.Config.ShellyConnected {
				info.Grid.Error = em3.LastError()
			}
			// Try to read data (non-blocking)
			select {
			case gridData := <-em3.C():
				if gridData != nil {
					info.Grid = *gridData
					info.Config.ShellyConnected = gridData.Valid
				}
			default:
				// No new data available - keep last state
			}
		}

		// Read battery data from BMV (VE.Direct)
		if bmv != nil {
			info.Config.BMVConnected = bmv.Connected()
			if bmv.Valid() {
				info.Battery = bmv.Data()
				info.Battery.Valid = true
				// Use BMV SOC if available (more accurate than inverter)
				modeCtrl.UpdateSOC(info.Battery.SOCPercent)
			} else {
				info.Battery.Valid = false
				info.Battery.Error = bmv.LastError()
			}
		}

		// Read inverter data from Victron (VE.Bus)
		if vbus != nil {
			status, err := vbus.Read()
			if err == nil && status.Valid {
				info.Inverter = core.InverterData{
					State:        status.DeviceStateName,
					BatVoltage:   status.BatVoltage,
					BatCurrent:   status.BatCurrent,
					BatPower:     status.BatPower,
					BatSOC:       status.BatSOC,
					BatCapacity:  webConfig.BatteryCapacity,
					BatRemaining: webConfig.BatteryCapacity * status.BatSOC / 100,
					ACInVoltage:  status.MainsVoltage,
					ACInCurrent:  status.MainsCurrent,
					ACOutVoltage: status.InvVoltage,
					ACOutCurrent: status.InvCurrent,
					Valid:        true,
				}
				// If BMV not available, use inverter SOC
				if !info.Battery.Valid {
					modeCtrl.UpdateSOC(status.BatSOC)
				}
				// If BMV is available, override inverter battery data with BMV data (more accurate)
				if info.Battery.Valid {
					info.Inverter.BatVoltage = info.Battery.VoltageV
					info.Inverter.BatCurrent = info.Battery.CurrentA
					info.Inverter.BatPower = info.Battery.PowerW
					info.Inverter.BatSOC = info.Battery.SOCPercent
					info.Inverter.BatRemaining = webConfig.BatteryCapacity * info.Battery.SOCPercent / 100
				}
			} else if err != nil {
				info.Inverter.Error = err.Error()
			}
		}

		// Determine effective grid power (real or simulated)
		gridPower := info.Grid.TotalPower
		simulatedLoadActive, simulatedLoadValue := modeCtrl.GetSimulatedLoad()
		useSimulatedLoad := simulatedLoadActive && !info.Grid.Valid

		if useSimulatedLoad {
			// Use simulated load as grid power when Shelly is disconnected
			// Positive simulated load = pretend house is consuming power = inverter will discharge
			gridPower = simulatedLoadValue
		}

		// Calculate setpoint based on current mode (always calculate)
		var setpoint float64
		setpoint, info.Mode = modeCtrl.Calculate(gridPower)

		// Run PID controller
		var output float64
		if pidCtrl != nil {
			// For non-PID modes, use the mode's calculated setpoint
			if modeCtrl.GetMode() != core.ModePID && modeCtrl.GetMode() != core.ModeManual {
				pidCtrl.SetSetpoint(setpoint)
			}

			// Update PID controller with effective grid power (real or simulated)
			output = pidCtrl.Update(gridPower)
			pidSetpoint, lastErr, _, _, _ := pidCtrl.State()

			info.PID = core.PIDData{
				Setpoint:  pidSetpoint,
				GridPower: gridPower,
				Error:     lastErr,
				Output:    output,
				Enabled:   info.Grid.Valid || useSimulatedLoad, // Enabled when grid valid OR simulated load active
			}
		}

		// Send command to inverter
		if vbus != nil && info.Inverter.Valid {
			cmdOutput := output

			if info.Mode.Override {
				// Override always works, even without grid data
				cmdOutput = info.Mode.OverrideVal
				if err := vbus.SetPower(cmdOutput); err != nil {
					log.Printf("Failed to set inverter power: %v", err)
				}
			} else if info.Grid.Valid || useSimulatedLoad {
				// Normal PID control when grid data valid OR simulated load active
				if err := vbus.SetPower(cmdOutput); err != nil {
					log.Printf("Failed to set inverter power: %v", err)
				}
			}
			// When grid is invalid and no override/simulated load, hold last state
		}

		// Broadcast to subscribers
		hub.Broadcast(info)

		// Add sample to RRD for historical data
		if rrdStore != nil {
			sample := rrd.Sample{
				Timestamp:  info.Timestamp,
				GridPower:  info.Grid.TotalPower,
				BatPower:   info.Inverter.BatPower,
				BatSOC:     info.Inverter.BatSOC,
				BatVoltage: info.Inverter.BatVoltage,
			}
			// Calculate solar power (simplified: grid import + battery discharge)
			// Positive grid = import, positive bat = discharge (to grid/load)
			// Solar = Load - Grid - Battery = -(Grid + Battery) for zero-grid
			if info.Grid.Valid && info.Inverter.Valid {
				sample.SolarPower = -info.Grid.TotalPower - info.Inverter.BatPower
				if sample.SolarPower < 0 {
					sample.SolarPower = 0 // Solar can't be negative
				}
			}
			rrdStore.Add(sample)
		}
	}
}
