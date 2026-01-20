package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/diebietse/invertergui/energymanager/core"
	"github.com/diebietse/invertergui/energymanager/discovery"
	"github.com/diebietse/invertergui/energymanager/modes"
	"github.com/diebietse/invertergui/energymanager/mqtt"
	"github.com/diebietse/invertergui/energymanager/pid"
	"github.com/diebietse/invertergui/energymanager/prometheus"
	"github.com/diebietse/invertergui/energymanager/serial"
	"github.com/diebietse/invertergui/energymanager/shelly"
	"github.com/diebietse/invertergui/energymanager/vebus"
	"github.com/diebietse/invertergui/energymanager/vedirect"
	"github.com/diebietse/invertergui/energymanager/webui"
)

var (
	// Server
	listenAddr = flag.String("listen", ":8080", "HTTP listen address")

	// Shelly Pro EM3
	shellyAddr       = flag.String("shelly.addr", "", "Shelly Pro EM3 address (host:port), auto-discover if empty")
	shellyInterval   = flag.Duration("shelly.interval", time.Second, "Shelly poll interval")
	shellyAutoConfig = flag.Bool("shelly.autoconfig", true, "Auto-discover and configure Shelly")

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

	// Auth (for web UI config/control pages)
	authUsername = flag.String("auth.username", "", "Admin username (default: admin)")
	authPassword = flag.String("auth.password", "", "Admin password (default: energy)")
)

func main() {
	flag.Parse()
	log.SetFlags(log.Ltime | log.Lshortfile)
	log.Println("Starting Energy Manager")

	// Auto-discover Shelly if not configured
	shellyAddress := *shellyAddr
	if shellyAddress == "" && *shellyAutoConfig {
		log.Println("Auto-discovering Shelly Pro EM3...")
		shellyAddress = discoverShelly()
	}

	if shellyAddress == "" {
		log.Println("Warning: No Shelly address configured. Use -shelly.addr or enable auto-discovery")
	}

	// Create hub for broadcasting data
	hub := core.NewHub()

	// Create mode controller
	modeCtrl := modes.New(modes.Config{
		MaxPower:        *victronMaxPower,
		BatteryCapacity: *batteryCapacity,
		Latitude:        *latitude,
		Longitude:       *longitude,
	})
	modeCtrl.SetMode(core.OperatingMode(*defaultMode))

	// Create web UI config
	webConfig := &webui.Config{
		ShellyAddr:      shellyAddress,
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
	}

	// Create Shelly EM3 driver
	var em3 *shelly.EM3
	if shellyAddress != "" {
		em3 = shelly.NewEM3(shellyAddress, *shellyInterval)
		if err := em3.Start(); err != nil {
			log.Printf("Warning: Failed to start Shelly EM3: %v", err)
			em3 = nil
		} else {
			log.Printf("Shelly Pro EM3 polling at %s", shellyAddress)
			webConfig.ShellyAddr = shellyAddress
		}
	}

	// Create Victron VE.Bus driver (inverter control)
	var vbus *vebus.VEBus
	if *victronEnabled {
		serialPort, err := serial.OpenPort(&serial.Config{
			Name:        *victronPort,
			Baud:        2400,
			ReadTimeout: time.Second,
		})
		if err != nil {
			log.Printf("Warning: Failed to open Victron port %s: %v", *victronPort, err)
		} else {
			vbus = vebus.New(serialPort, *victronMaxPower)
			if err := vbus.Connect(); err != nil {
				log.Printf("Warning: Failed to connect to Victron: %v", err)
				serialPort.Close()
				vbus = nil
			} else {
				log.Printf("Victron VE.Bus connected via %s (max %.0fW)", *victronPort, *victronMaxPower)
			}
		}
	}

	// Create Victron BMV driver (battery monitor via VE.Direct)
	var bmv *vedirect.BMV
	if *bmvEnabled {
		bmv = vedirect.NewBMV(*bmvPort)
		if err := bmv.Connect(); err != nil {
			log.Printf("Warning: Failed to connect to BMV: %v", err)
			bmv = nil
		} else {
			log.Printf("Victron BMV connected via %s", *bmvPort)
		}
	}

	// Create PID controller
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
	go controlLoop(hub, em3, vbus, bmv, pidCtrl, modeCtrl, webConfig)

	// Setup HTTP handlers
	mux := http.NewServeMux()
	webHandler := webui.New(hub, modeCtrl, webConfig)
	webHandler.RegisterRoutes(mux)

	promHandler := prometheus.New(hub)
	mux.Handle("/metrics", promHandler)

	// Start MQTT if enabled
	if *mqttEnabled {
		mqttPub := mqtt.New(hub, mqtt.Config{
			Broker:   *mqttBroker,
			Topic:    *mqttTopic,
			ClientID: *mqttClientID,
			Username: *mqttUsername,
			Password: *mqttPassword,
		})
		if err := mqttPub.Start(); err != nil {
			log.Printf("Warning: Failed to start MQTT: %v", err)
		} else {
			defer mqttPub.Stop()
			log.Printf("MQTT publishing to %s topic %s", *mqttBroker, *mqttTopic)
		}
	}

	// Start HTTP server
	go func() {
		log.Printf("HTTP server listening on %s", *listenAddr)
		if err := http.ListenAndServe(*listenAddr, mux); err != nil {
			log.Fatal(err)
		}
	}()

	// Wait for shutdown signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	log.Println("Shutting down...")

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

func controlLoop(hub *core.Hub, em3 *shelly.EM3, vbus *vebus.VEBus, bmv *vedirect.BMV, pidCtrl *pid.Controller, modeCtrl *modes.Controller, webConfig *webui.Config) {
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
			select {
			case gridData := <-em3.C():
				if gridData != nil {
					info.Grid = *gridData
				}
			default:
				// No new data available
			}
		}

		// Read battery data from BMV (VE.Direct)
		if bmv != nil && bmv.Valid() {
			info.Battery = bmv.Data()
			info.Battery.Valid = true
			// Use BMV SOC if available (more accurate than inverter)
			modeCtrl.UpdateSOC(info.Battery.SOCPercent)
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

		// Calculate setpoint based on current mode
		var setpoint float64
		if info.Grid.Valid {
			setpoint, info.Mode = modeCtrl.Calculate(info.Grid.TotalPower)
		}

		// Run PID controller if in PID mode or need zero-grid control
		var output float64
		if pidCtrl != nil && info.Grid.Valid {
			// For non-PID modes, use the mode's calculated setpoint
			if modeCtrl.GetMode() != core.ModePID && modeCtrl.GetMode() != core.ModeManual {
				pidCtrl.SetSetpoint(setpoint)
			}

			output = pidCtrl.Update(info.Grid.TotalPower)
			pidSetpoint, lastErr, _, _, enabled := pidCtrl.State()

			info.PID = core.PIDData{
				Setpoint:  pidSetpoint,
				GridPower: info.Grid.TotalPower,
				Error:     lastErr,
				Output:    output,
				Enabled:   enabled,
			}
		}

		// Send command to inverter
		if vbus != nil && info.Inverter.Valid {
			// Use mode output if override, otherwise PID output
			cmdOutput := output
			if info.Mode.Override {
				cmdOutput = info.Mode.OverrideVal
			}

			if err := vbus.SetPower(cmdOutput); err != nil {
				log.Printf("Failed to set inverter power: %v", err)
			}
		}

		// Broadcast to subscribers
		hub.Broadcast(info)
	}
}
