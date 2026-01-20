package webui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/diebietse/invertergui/energymanager/auth"
	"github.com/diebietse/invertergui/energymanager/core"
	"github.com/diebietse/invertergui/energymanager/discovery"
	"github.com/diebietse/invertergui/energymanager/modes"
)


// InitStatus tracks initialization progress
type InitStatus struct {
	Stage       string `json:"stage"`
	Description string `json:"description"`
	Ready       bool   `json:"ready"`
	Error       string `json:"error,omitempty"`
}

// ServiceCallbacks holds callbacks for runtime service reconfiguration
type ServiceCallbacks struct {
	// ReconfigureMQTT is called when MQTT config changes
	// Returns error if reconfiguration fails
	ReconfigureMQTT func(enabled bool, broker, topic, clientID, username, password string) error
	// ReconfigureInfluxDB is called when InfluxDB config changes
	ReconfigureInfluxDB func(enabled bool, url, database, username, password, token, org string, interval time.Duration) error
}

// Handler serves the web UI
type Handler struct {
	hub        *core.Hub
	modeCtrl   *modes.Controller
	config     *Config
	auth       *auth.Authenticator
	callbacks  *ServiceCallbacks
	mu         sync.RWMutex
	latest     *core.EnergyInfo
	initStatus *InitStatus
}

// Config holds runtime configuration
type Config struct {
	ShellyAddr      string  `json:"shelly_addr"`
	VictronPort     string  `json:"victron_port"`
	BMVPort         string  `json:"bmv_port"`
	MaxPower        float64 `json:"max_power"`
	BatteryCapacity float64 `json:"battery_capacity"`
	Latitude        float64 `json:"latitude"`
	Longitude       float64 `json:"longitude"`

	// PID config
	PIDKp       float64 `json:"pid_kp"`
	PIDKi       float64 `json:"pid_ki"`
	PIDKd       float64 `json:"pid_kd"`
	PIDSetpoint float64 `json:"pid_setpoint"`

	// TOU config
	TOUSchedule core.TOUSchedule `json:"tou_schedule"`

	// Mode schedule config (for automatic mode switching)
	ModeSchedule core.ModeSchedule `json:"mode_schedule"`

	// Solar config
	SolarConfig core.SolarConfig `json:"solar_config"`

	// Battery-save config
	BatterySaveConfig core.BatterySaveConfig `json:"battery_save_config"`

	// Auth config (for protected endpoints)
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`

	// MQTT config
	MQTTEnabled  bool   `json:"mqtt_enabled"`
	MQTTBroker   string `json:"mqtt_broker"`
	MQTTTopic    string `json:"mqtt_topic"`
	MQTTClientID string `json:"mqtt_client_id"`
	MQTTUsername string `json:"mqtt_username,omitempty"`
	MQTTPassword string `json:"mqtt_password,omitempty"`

	// InfluxDB config
	InfluxEnabled  bool          `json:"influx_enabled"`
	InfluxURL      string        `json:"influx_url"`
	InfluxDatabase string        `json:"influx_database"`
	InfluxUsername string        `json:"influx_username,omitempty"`
	InfluxPassword string        `json:"influx_password,omitempty"`
	InfluxToken    string        `json:"influx_token,omitempty"`
	InfluxOrg      string        `json:"influx_org,omitempty"`
	InfluxInterval time.Duration `json:"influx_interval"`
}

// New creates a new web UI handler
func New(hub *core.Hub, modeCtrl *modes.Controller, cfg *Config) *Handler {
	h := &Handler{
		hub:      hub,
		modeCtrl: modeCtrl,
		config:   cfg,
		auth:     auth.New(cfg.Username, cfg.Password), // Fallback to config if system auth fails
		initStatus: &InitStatus{
			Stage:       "starting",
			Description: "Starting Energy Manager...",
			Ready:       false,
		},
	}
	go h.updateLoop()
	return h
}

// SetCallbacks sets the service reconfiguration callbacks
func (h *Handler) SetCallbacks(cb *ServiceCallbacks) {
	h.mu.Lock()
	h.callbacks = cb
	h.mu.Unlock()
}

// SetInitStatus updates the initialization status
func (h *Handler) SetInitStatus(stage, description string, ready bool, err string) {
	h.mu.Lock()
	h.initStatus = &InitStatus{
		Stage:       stage,
		Description: description,
		Ready:       ready,
		Error:       err,
	}
	h.mu.Unlock()
}

// GetInitStatus returns the current initialization status
func (h *Handler) GetInitStatus() *InitStatus {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.initStatus
}

func (h *Handler) updateLoop() {
	sub := h.hub.Subscribe()
	for info := range sub.C() {
		h.mu.Lock()
		h.latest = info
		h.mu.Unlock()
	}
}

// requireAuth wraps a handler with basic authentication
// Authenticates against OpenWRT's root password (same as LuCI)
// Falls back to configured credentials if system auth unavailable
func (h *Handler) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || !h.auth.Authenticate(user, pass) {
			w.Header().Set("WWW-Authenticate", `Basic realm="Energy Manager - Use router root password"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// RegisterRoutes registers all HTTP routes
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	// Public endpoints - no auth required
	mux.HandleFunc("/", h.serveDashboard)
	mux.HandleFunc("/api/status", h.serveStatus)
	mux.HandleFunc("/api/init", h.handleInitStatus)

	// Protected endpoints - require authentication
	mux.HandleFunc("/config", h.requireAuth(h.serveConfig))
	mux.HandleFunc("/api/config", h.requireAuth(h.handleConfig))
	mux.HandleFunc("/api/mode", h.requireAuth(h.handleMode))
	mux.HandleFunc("/api/override", h.requireAuth(h.handleOverride))
	mux.HandleFunc("/api/discover", h.requireAuth(h.handleDiscover))
	mux.HandleFunc("/api/tou", h.requireAuth(h.handleTOU))
	mux.HandleFunc("/api/schedule", h.requireAuth(h.handleSchedule))
	mux.HandleFunc("/api/ports", h.requireAuth(h.handlePorts))
}

func (h *Handler) handleInitStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	h.mu.RLock()
	status := h.initStatus
	h.mu.RUnlock()
	json.NewEncoder(w).Encode(status)
}

func (h *Handler) serveDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && r.URL.Path != "/index.html" && r.URL.Path != "/dashboard" {
		http.NotFound(w, r)
		return
	}

	h.mu.RLock()
	info := h.latest
	h.mu.RUnlock()

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, h.dashboardHTML(info))
}

func (h *Handler) serveConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, h.configHTML())
}

func (h *Handler) serveStatus(w http.ResponseWriter, r *http.Request) {
	h.mu.RLock()
	info := h.latest
	h.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	if info == nil {
		w.Write([]byte(`{"error":"no data"}`))
		return
	}
	json.NewEncoder(w).Encode(info)
}

func (h *Handler) handleConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if r.Method == "GET" {
		json.NewEncoder(w).Encode(h.config)
		return
	}

	if r.Method == "POST" {
		var cfg Config
		if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		// Check if MQTT config changed
		mqttChanged := h.config.MQTTEnabled != cfg.MQTTEnabled ||
			h.config.MQTTBroker != cfg.MQTTBroker ||
			h.config.MQTTTopic != cfg.MQTTTopic ||
			h.config.MQTTClientID != cfg.MQTTClientID ||
			h.config.MQTTUsername != cfg.MQTTUsername ||
			h.config.MQTTPassword != cfg.MQTTPassword

		// Check if InfluxDB config changed
		influxChanged := h.config.InfluxEnabled != cfg.InfluxEnabled ||
			h.config.InfluxURL != cfg.InfluxURL ||
			h.config.InfluxDatabase != cfg.InfluxDatabase ||
			h.config.InfluxUsername != cfg.InfluxUsername ||
			h.config.InfluxPassword != cfg.InfluxPassword ||
			h.config.InfluxToken != cfg.InfluxToken ||
			h.config.InfluxOrg != cfg.InfluxOrg ||
			h.config.InfluxInterval != cfg.InfluxInterval

		h.config = &cfg

		// Apply config to mode controller
		if h.modeCtrl != nil {
			h.modeCtrl.SetTOUSchedule(cfg.TOUSchedule)
			h.modeCtrl.SetSolarConfig(cfg.SolarConfig)
			h.modeCtrl.SetBatterySaveConfig(cfg.BatterySaveConfig)
			h.modeCtrl.SetModeSchedule(cfg.ModeSchedule)
		}

		// Apply MQTT config change
		if mqttChanged && h.callbacks != nil && h.callbacks.ReconfigureMQTT != nil {
			if err := h.callbacks.ReconfigureMQTT(
				cfg.MQTTEnabled,
				cfg.MQTTBroker,
				cfg.MQTTTopic,
				cfg.MQTTClientID,
				cfg.MQTTUsername,
				cfg.MQTTPassword,
			); err != nil {
				json.NewEncoder(w).Encode(map[string]string{"status": "error", "error": "MQTT: " + err.Error()})
				return
			}
		}

		// Apply InfluxDB config change
		if influxChanged && h.callbacks != nil && h.callbacks.ReconfigureInfluxDB != nil {
			if err := h.callbacks.ReconfigureInfluxDB(
				cfg.InfluxEnabled,
				cfg.InfluxURL,
				cfg.InfluxDatabase,
				cfg.InfluxUsername,
				cfg.InfluxPassword,
				cfg.InfluxToken,
				cfg.InfluxOrg,
				cfg.InfluxInterval,
			); err != nil {
				json.NewEncoder(w).Encode(map[string]string{"status": "error", "error": "InfluxDB: " + err.Error()})
				return
			}
		}

		// Persist to UCI config
		if err := h.saveToUCI(&cfg); err != nil {
			json.NewEncoder(w).Encode(map[string]string{"status": "error", "error": err.Error()})
			return
		}
		w.Write([]byte(`{"status":"ok"}`))
		return
	}

	http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
}

func (h *Handler) handleSchedule(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if r.Method == "GET" {
		json.NewEncoder(w).Encode(h.config.ModeSchedule)
		return
	}

	if r.Method == "POST" {
		var schedule core.ModeSchedule
		if err := json.NewDecoder(r.Body).Decode(&schedule); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		h.config.ModeSchedule = schedule
		h.modeCtrl.SetModeSchedule(schedule)
		w.Write([]byte(`{"status":"ok"}`))
		return
	}

	http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
}

func (h *Handler) handlePorts(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	ports := detectSerialPorts()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"ports": ports,
	})
}

// detectSerialPorts scans /dev/ for serial port devices
func detectSerialPorts() []string {
	var ports []string

	// Common serial port patterns
	patterns := []string{
		"/dev/ttyUSB*",
		"/dev/ttyACM*",
		"/dev/ttyS*",
		"/dev/ttyAMA*",
		"/dev/serial/by-id/*",
		"/dev/serial/by-path/*",
	}

	seen := make(map[string]bool)

	for _, pattern := range patterns {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			continue
		}
		for _, match := range matches {
			// Resolve symlinks to get actual device
			resolved, err := filepath.EvalSymlinks(match)
			if err != nil {
				resolved = match
			}

			// Skip if we've already seen this device
			if seen[resolved] {
				continue
			}
			seen[resolved] = true

			// Check if it's a character device (serial port)
			info, err := os.Stat(match)
			if err != nil {
				continue
			}
			mode := info.Mode()
			if mode&os.ModeCharDevice != 0 || mode&os.ModeSymlink != 0 {
				// For by-id and by-path, include the full path for better identification
				if strings.Contains(match, "/by-id/") || strings.Contains(match, "/by-path/") {
					ports = append(ports, match)
				} else {
					ports = append(ports, resolved)
				}
			}
		}
	}

	// Sort for consistent ordering
	sort.Strings(ports)

	// If no ports found, add common defaults
	if len(ports) == 0 {
		ports = []string{"/dev/ttyUSB0", "/dev/ttyUSB1", "/dev/ttyACM0"}
	}

	return ports
}

func (h *Handler) handleMode(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if r.Method == "GET" {
		mode := h.modeCtrl.GetMode()
		json.NewEncoder(w).Encode(map[string]string{"mode": string(mode)})
		return
	}

	if r.Method == "POST" {
		var req struct {
			Mode string `json:"mode"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		h.modeCtrl.SetMode(core.OperatingMode(req.Mode))
		w.Write([]byte(`{"status":"ok"}`))
		return
	}

	http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
}

func (h *Handler) handleOverride(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if r.Method == "POST" {
		var req struct {
			Enabled bool    `json:"enabled"`
			Value   float64 `json:"value"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		h.modeCtrl.SetOverride(req.Enabled, req.Value)
		w.Write([]byte(`{"status":"ok"}`))
		return
	}

	http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
}

func (h *Handler) handleDiscover(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	scanner := discovery.NewScanner()
	devices, err := scanner.ScanLocalNetwork()
	if err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": err.Error(),
		})
		return
	}

	em3Devices := scanner.FindEM3Devices(devices)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"all_devices": devices,
		"em3_devices": em3Devices,
	})
}

func (h *Handler) handleTOU(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if r.Method == "GET" {
		json.NewEncoder(w).Encode(h.config.TOUSchedule)
		return
	}

	if r.Method == "POST" {
		var schedule core.TOUSchedule
		if err := json.NewDecoder(r.Body).Decode(&schedule); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		h.config.TOUSchedule = schedule
		h.modeCtrl.SetTOUSchedule(schedule)
		w.Write([]byte(`{"status":"ok"}`))
		return
	}

	http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
}

// saveToUCI persists the configuration to /etc/config/energymanager using UCI
func (h *Handler) saveToUCI(cfg *Config) error {
	// Check if UCI is available (OpenWRT)
	if _, err := exec.LookPath("uci"); err != nil {
		// Not on OpenWRT, skip UCI persistence
		return nil
	}

	// Helper to run uci commands
	uci := func(args ...string) error {
		cmd := exec.Command("uci", args...)
		return cmd.Run()
	}

	// Set main config
	uci("set", "energymanager.main.shelly_addr="+cfg.ShellyAddr)
	uci("set", "energymanager.victron.port="+cfg.VictronPort)
	uci("set", "energymanager.bmv.port="+cfg.BMVPort)
	uci("set", "energymanager.victron.maxpower="+strconv.FormatFloat(cfg.MaxPower, 'f', 0, 64))
	uci("set", "energymanager.battery.capacity="+strconv.FormatFloat(cfg.BatteryCapacity, 'f', 1, 64))
	uci("set", "energymanager.location.latitude="+strconv.FormatFloat(cfg.Latitude, 'f', 6, 64))
	uci("set", "energymanager.location.longitude="+strconv.FormatFloat(cfg.Longitude, 'f', 6, 64))

	// PID config
	uci("set", "energymanager.pid.kp="+strconv.FormatFloat(cfg.PIDKp, 'f', 2, 64))
	uci("set", "energymanager.pid.ki="+strconv.FormatFloat(cfg.PIDKi, 'f', 2, 64))
	uci("set", "energymanager.pid.kd="+strconv.FormatFloat(cfg.PIDKd, 'f', 2, 64))
	uci("set", "energymanager.pid.setpoint="+strconv.FormatFloat(cfg.PIDSetpoint, 'f', 0, 64))

	// Solar config
	zeroExport := "0"
	if cfg.SolarConfig.ZeroExport {
		zeroExport = "1"
	}
	uci("set", "energymanager.solar.zero_export="+zeroExport)
	uci("set", "energymanager.solar.export_limit="+strconv.FormatFloat(cfg.SolarConfig.ExportLimit, 'f', 0, 64))
	uci("set", "energymanager.solar.min_soc="+strconv.FormatFloat(cfg.SolarConfig.MinSOC, 'f', 0, 64))
	uci("set", "energymanager.solar.max_soc="+strconv.FormatFloat(cfg.SolarConfig.MaxSOC, 'f', 0, 64))

	// Battery save config
	uci("set", "energymanager.battery_save.min_soc="+strconv.FormatFloat(cfg.BatterySaveConfig.MinSOC, 'f', 0, 64))
	uci("set", "energymanager.battery_save.target_soc_sunrise="+strconv.FormatFloat(cfg.BatterySaveConfig.TargetSOC, 'f', 0, 64))
	uci("set", "energymanager.battery_save.sunrise_offset_minutes="+strconv.Itoa(cfg.BatterySaveConfig.SunriseOffset))

	// Mode schedule - store as JSON
	if scheduleJSON, err := json.Marshal(cfg.ModeSchedule); err == nil {
		scheduleEnabled := "0"
		if cfg.ModeSchedule.Enabled {
			scheduleEnabled = "1"
		}
		uci("set", "energymanager.schedule.enabled="+scheduleEnabled)
		// Store periods as JSON in a single option
		if len(cfg.ModeSchedule.Periods) > 0 {
			uci("set", "energymanager.schedule.periods="+string(scheduleJSON))
		}
	}

	// MQTT config
	mqttEnabled := "0"
	if cfg.MQTTEnabled {
		mqttEnabled = "1"
	}
	uci("set", "energymanager.mqtt.enabled="+mqttEnabled)
	uci("set", "energymanager.mqtt.broker="+cfg.MQTTBroker)
	uci("set", "energymanager.mqtt.topic="+cfg.MQTTTopic)
	uci("set", "energymanager.mqtt.client_id="+cfg.MQTTClientID)
	uci("set", "energymanager.mqtt.username="+cfg.MQTTUsername)
	uci("set", "energymanager.mqtt.password="+cfg.MQTTPassword)

	// InfluxDB config
	influxEnabled := "0"
	if cfg.InfluxEnabled {
		influxEnabled = "1"
	}
	uci("set", "energymanager.influxdb.enabled="+influxEnabled)
	uci("set", "energymanager.influxdb.url="+cfg.InfluxURL)
	uci("set", "energymanager.influxdb.database="+cfg.InfluxDatabase)
	uci("set", "energymanager.influxdb.username="+cfg.InfluxUsername)
	uci("set", "energymanager.influxdb.password="+cfg.InfluxPassword)
	uci("set", "energymanager.influxdb.token="+cfg.InfluxToken)
	uci("set", "energymanager.influxdb.org="+cfg.InfluxOrg)
	uci("set", "energymanager.influxdb.interval="+cfg.InfluxInterval.String())

	// Commit changes
	if err := uci("commit", "energymanager"); err != nil {
		return fmt.Errorf("uci commit failed: %w", err)
	}

	return nil
}

func (h *Handler) dashboardHTML(info *core.EnergyInfo) string {
	gridPower := "---"
	gridClass := ""
	gridStatus := "Offline"
	gridStatusClass := "offline"
	gridError := ""
	phaseHTML := ""
	invState := "---"
	invPower := "---"
	invACInVoltage := "---"
	invACInCurrent := "---"
	invACOutVoltage := "---"
	invACOutCurrent := "---"
	batVoltage := "---"
	batSOC := "---"
	batCurrent := "---"
	batPower := "---"
	batChargedKWh := "---"
	batDischargedKWh := "---"
	batTimeToGo := "---"
	batProduct := "---"
	batConsumedAh := "---"
	batChargeCycles := "---"
	batMinVoltage := "---"
	batMaxVoltage := "---"
	batTimeSinceFullCharge := "---"
	batTemperature := "---"
	batAlarm := ""
	batFirmware := "---"
	batSerial := "---"
	bmvConnected := false
	modeStr := "---"
	modeDesc := "---"
	pidOutput := "---"
	overrideValue := 0
	overrideActive := false
	timestamp := "---"

	if info != nil {
		timestamp = info.Timestamp.Format(time.RFC1123)

		if info.Grid.Valid {
			gridPower = fmt.Sprintf("%.0f W", info.Grid.TotalPower)
			gridStatus = "Online"
			gridStatusClass = "online"
			if info.Grid.TotalPower > 50 {
				gridClass = "importing"
			} else if info.Grid.TotalPower < -50 {
				gridClass = "exporting"
			} else {
				gridClass = "balanced"
			}
			for i, p := range info.Grid.Phases {
				phaseHTML += fmt.Sprintf(`
				<div class="phase">
					<div class="phase-label">Phase %c</div>
					<div class="phase-main">%.1f V | %.2f A | %.0f W</div>
					<div class="phase-detail">%.0f VA | PF: %.2f | %.1f Hz</div>
				</div>`, 'A'+i, p.Voltage, p.Current, p.ActivePower, p.ApparentPower, p.PowerFactor, p.Frequency)
			}
		} else if info.Grid.Error != "" {
			gridError = info.Grid.Error
			gridStatus = "Error"
			gridStatusClass = "error"
		}

		if info.Inverter.Valid {
			invState = info.Inverter.State
			invPower = fmt.Sprintf("%.0f W", info.Inverter.BatPower)
			batVoltage = fmt.Sprintf("%.1f V", info.Inverter.BatVoltage)
			batSOC = fmt.Sprintf("%.0f%%", info.Inverter.BatSOC)
			if info.Inverter.ACInVoltage > 0 {
				invACInVoltage = fmt.Sprintf("%.1f V", info.Inverter.ACInVoltage)
				invACInCurrent = fmt.Sprintf("%.1f A", info.Inverter.ACInCurrent)
			}
			if info.Inverter.ACOutVoltage > 0 {
				invACOutVoltage = fmt.Sprintf("%.1f V", info.Inverter.ACOutVoltage)
				invACOutCurrent = fmt.Sprintf("%.1f A", info.Inverter.ACOutCurrent)
			}
		}

		// Battery monitor data (from BMV via VE.Direct)
		if info.Battery.Valid {
			bmvConnected = true
			batVoltage = fmt.Sprintf("%.2f V", info.Battery.VoltageV)
			batCurrent = fmt.Sprintf("%.2f A", info.Battery.CurrentA)
			batPower = fmt.Sprintf("%.0f W", info.Battery.PowerW)
			batSOC = fmt.Sprintf("%.1f%%", info.Battery.SOCPercent)
			batChargedKWh = fmt.Sprintf("%.2f kWh", info.Battery.ChargedKWh)
			batDischargedKWh = fmt.Sprintf("%.2f kWh", info.Battery.DischargedKWh)
			batConsumedAh = fmt.Sprintf("%.2f Ah", info.Battery.ConsumedAh)
			batChargeCycles = fmt.Sprintf("%d", info.Battery.ChargeCycles)
			batMinVoltage = fmt.Sprintf("%.2f V", info.Battery.MinVoltageV)
			batMaxVoltage = fmt.Sprintf("%.2f V", info.Battery.MaxVoltageV)
			if info.Battery.TimeToGoMin > 0 {
				hours := info.Battery.TimeToGoMin / 60
				mins := info.Battery.TimeToGoMin % 60
				batTimeToGo = fmt.Sprintf("%dh %dm", hours, mins)
			} else if info.Battery.TimeToGoMin == -1 {
				batTimeToGo = "Infinite"
			}
			if info.Battery.SecondsSinceFullCharge > 0 {
				hours := info.Battery.SecondsSinceFullCharge / 3600
				mins := (info.Battery.SecondsSinceFullCharge % 3600) / 60
				batTimeSinceFullCharge = fmt.Sprintf("%dh %dm", hours, mins)
			}
			if info.Battery.HasTemperature {
				batTemperature = fmt.Sprintf("%.1f°C", info.Battery.TemperatureC)
			}
			if info.Battery.AlarmActive {
				batAlarm = info.Battery.AlarmReason
			}
			batProduct = info.Battery.ProductName
			batFirmware = info.Battery.FirmwareVersion
			batSerial = info.Battery.SerialNumber
		}

		modeStr = string(info.Mode.Current)
		modeDesc = info.Mode.Description
		pidOutput = fmt.Sprintf("%.0f W", info.PID.Output)
		overrideValue = int(info.Mode.OverrideVal)
		overrideActive = info.Mode.Override
	}

	bmvClass := ""
	if bmvConnected {
		bmvClass = "connected"
	}

	return fmt.Sprintf(`<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Energy Manager</title>
<style>
%s
</style>
</head>
<body>
<nav class="navbar">
    <div class="nav-brand">Energy Manager</div>
    <div class="nav-links">
        <a href="/" class="active">Dashboard</a>
        <a href="/config">Config</a>
        <a href="/metrics">Metrics</a>
        <a id="luci-link" href="/cgi-bin/luci/" target="_blank">LuCI</a>
        <a id="glinet-link" href="/" target="_blank">GL.iNet</a>
    </div>
</nav>

<div id="init-status" class="init-status">
    <div class="init-spinner"></div>
    <span id="init-message">Initializing...</span>
</div>

<main class="container">
    <div class="grid-container">
        <div class="card grid-card">
            <div class="card-header">
                <h2>Grid Power</h2>
                <span class="status-badge %s">%s</span>
            </div>
            <div class="big %s" id="grid-power">%s</div>
            <div class="grid-error" id="grid-error">%s</div>
            <div class="phases">%s</div>
        </div>

        <div class="card inverter-card">
            <h2>Inverter</h2>
            <div class="stat-row"><span>State:</span><span id="inv-state">%s</span></div>
            <div class="stat-row"><span>Battery Power:</span><span id="inv-power">%s</span></div>
            <div class="stat-row"><span>Battery Voltage:</span><span id="bat-voltage">%s</span></div>
            <div class="stat-row"><span>SOC:</span><span id="bat-soc">%s</span></div>
            <div class="soc-bar"><div class="soc-fill" id="soc-fill" style="width:%s"></div></div>
            <details class="details-section">
                <summary>AC Details</summary>
                <div class="stat-row"><span>AC In Voltage:</span><span id="inv-ac-in-v">%s</span></div>
                <div class="stat-row"><span>AC In Current:</span><span id="inv-ac-in-a">%s</span></div>
                <div class="stat-row"><span>AC Out Voltage:</span><span id="inv-ac-out-v">%s</span></div>
                <div class="stat-row"><span>AC Out Current:</span><span id="inv-ac-out-a">%s</span></div>
            </details>
        </div>

        <div class="card battery-card %s">
            <h2>Battery Monitor (BMV)</h2>
            <div class="stat-row"><span>Device:</span><span id="bmv-product">%s</span></div>
            <div class="stat-row"><span>Voltage:</span><span id="bmv-voltage">%s</span></div>
            <div class="stat-row"><span>Current:</span><span id="bmv-current">%s</span></div>
            <div class="stat-row"><span>Power:</span><span id="bmv-power">%s</span></div>
            <div class="stat-row"><span>SOC:</span><span id="bmv-soc">%s</span></div>
            <div class="stat-row"><span>Time to Go:</span><span id="bmv-ttg">%s</span></div>
            <div class="bmv-alarm" id="bmv-alarm">%s</div>
            <details class="details-section">
                <summary>Energy Totals</summary>
                <div class="stat-row"><span>Charged:</span><span id="bmv-charged">%s</span></div>
                <div class="stat-row"><span>Discharged:</span><span id="bmv-discharged">%s</span></div>
                <div class="stat-row"><span>Consumed:</span><span id="bmv-consumed">%s</span></div>
                <div class="stat-row"><span>Charge Cycles:</span><span id="bmv-cycles">%s</span></div>
            </details>
            <details class="details-section">
                <summary>History</summary>
                <div class="stat-row"><span>Min Voltage:</span><span id="bmv-min-v">%s</span></div>
                <div class="stat-row"><span>Max Voltage:</span><span id="bmv-max-v">%s</span></div>
                <div class="stat-row"><span>Since Full:</span><span id="bmv-since-full">%s</span></div>
                <div class="stat-row"><span>Temperature:</span><span id="bmv-temp">%s</span></div>
            </details>
            <details class="details-section">
                <summary>Device Info</summary>
                <div class="stat-row"><span>Firmware:</span><span id="bmv-fw">%s</span></div>
                <div class="stat-row"><span>Serial:</span><span id="bmv-serial">%s</span></div>
            </details>
        </div>

        <div class="card mode-card">
            <h2>Mode Control</h2>
            <div class="mode-selector">
                <button onclick="setMode('scheduled')" class="%s">Scheduled</button>
                <button onclick="setMode('pid')" class="%s">PID (Zero Grid)</button>
                <button onclick="setMode('tou')" class="%s">Time of Use</button>
                <button onclick="setMode('solar')" class="%s">Solar</button>
                <button onclick="setMode('battery_save')" class="%s">Battery Save</button>
            </div>
            <div class="stat-row"><span>Active:</span><span id="mode-name">%s</span></div>
            <div class="stat-row"><span>Status:</span><span id="mode-desc">%s</span></div>
            <div class="stat-row"><span>Output:</span><span id="pid-output">%s</span></div>

            <h3>Manual Override</h3>
            <div class="override-control">
                <input type="range" id="override-slider" min="-5000" max="5000" value="%d" oninput="updateOverrideDisplay()">
                <span id="override-value">%d W</span>
                <button onclick="setOverride(true)">Apply</button>
                <button onclick="setOverride(false)">Clear</button>
            </div>
        </div>

        <div class="card links-card">
            <h2>Quick Links</h2>
            <a href="/api/status" class="link-button">JSON API</a>
            <a href="/metrics" class="link-button">Prometheus</a>
            <a href="/config" class="link-button">Configuration</a>
            <a id="luci-admin-link" href="/cgi-bin/luci/" class="link-button" target="_blank">Router Admin (LuCI)</a>
        </div>
    </div>

    <div class="timestamp">Updated: %s</div>
</main>

<script>
%s
</script>
</body>
</html>`,
		dashboardCSS(),
		gridStatusClass, gridStatus, gridClass, gridPower, gridError, phaseHTML,
		invState, invPower, batVoltage, batSOC, batSOC,
		invACInVoltage, invACInCurrent, invACOutVoltage, invACOutCurrent,
		bmvClass, batProduct, batVoltage, batCurrent, batPower, batSOC, batTimeToGo, batAlarm,
		batChargedKWh, batDischargedKWh, batConsumedAh, batChargeCycles,
		batMinVoltage, batMaxVoltage, batTimeSinceFullCharge, batTemperature,
		batFirmware, batSerial,
		modeButtonClass("scheduled", modeStr),
		modeButtonClass("pid", modeStr),
		modeButtonClass("tou", modeStr),
		modeButtonClass("solar", modeStr),
		modeButtonClass("battery_save", modeStr),
		modeStr, modeDesc, pidOutput,
		overrideValue, overrideValue,
		timestamp,
		dashboardJS(overrideActive),
	)
}

func modeButtonClass(mode, current string) string {
	if mode == current {
		return "active"
	}
	return ""
}

func dashboardCSS() string {
	return `
* { box-sizing: border-box; margin: 0; padding: 0; }
body { font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', sans-serif; background: #0f172a; color: #e2e8f0; min-height: 100vh; }
.navbar { background: #1e293b; padding: 1rem 2rem; display: flex; justify-content: space-between; align-items: center; border-bottom: 1px solid #334155; }
.nav-brand { font-size: 1.25rem; font-weight: 600; color: #38bdf8; }
.nav-links a { color: #94a3b8; text-decoration: none; margin-left: 1.5rem; padding: 0.5rem 1rem; border-radius: 0.375rem; transition: all 0.2s; }
.nav-links a:hover, .nav-links a.active { color: #f8fafc; background: #334155; }
.container { padding: 2rem; max-width: 1400px; margin: 0 auto; }
.grid-container { display: grid; gap: 1.5rem; grid-template-columns: repeat(auto-fit, minmax(300px, 1fr)); }
.card { background: #1e293b; border-radius: 0.75rem; padding: 1.5rem; border: 1px solid #334155; }
h2 { font-size: 1rem; color: #94a3b8; margin-bottom: 1rem; text-transform: uppercase; letter-spacing: 0.05em; }
.card-header { display: flex; justify-content: space-between; align-items: center; margin-bottom: 1rem; }
.card-header h2 { margin-bottom: 0; }
.status-badge { font-size: 0.75rem; padding: 0.25rem 0.5rem; border-radius: 0.25rem; font-weight: 500; }
.status-badge.online { background: #065f46; color: #6ee7b7; }
.status-badge.offline { background: #7f1d1d; color: #fca5a5; }
.status-badge.error { background: #78350f; color: #fcd34d; }
.grid-error { font-size: 0.75rem; color: #fca5a5; margin-bottom: 0.5rem; word-break: break-all; }
.details-section { margin-top: 0.75rem; border-top: 1px solid #334155; padding-top: 0.5rem; }
.details-section summary { cursor: pointer; color: #64748b; font-size: 0.875rem; padding: 0.5rem 0; }
.details-section summary:hover { color: #94a3b8; }
.details-section[open] summary { color: #38bdf8; }
.phase-main { font-size: 0.875rem; }
.phase-detail { font-size: 0.75rem; color: #64748b; margin-top: 0.25rem; }
.bmv-alarm { color: #f87171; font-size: 0.875rem; padding: 0.5rem; background: #7f1d1d; border-radius: 0.25rem; margin: 0.5rem 0; }
.bmv-alarm:empty { display: none; }
h3 { font-size: 0.875rem; color: #64748b; margin: 1rem 0 0.5rem; }
.big { font-size: 3rem; font-weight: 700; line-height: 1; margin-bottom: 1rem; }
.importing { color: #f87171; }
.exporting { color: #4ade80; }
.balanced { color: #facc15; }
.phases { display: flex; gap: 0.75rem; flex-wrap: wrap; }
.phase { flex: 1; min-width: 80px; background: #0f172a; padding: 0.75rem; border-radius: 0.5rem; text-align: center; font-size: 0.875rem; }
.phase-label { color: #64748b; font-size: 0.75rem; margin-bottom: 0.25rem; }
.stat-row { display: flex; justify-content: space-between; padding: 0.5rem 0; border-bottom: 1px solid #334155; }
.stat-row:last-child { border-bottom: none; }
.soc-bar { height: 8px; background: #334155; border-radius: 4px; overflow: hidden; margin-top: 1rem; }
.soc-fill { height: 100%; background: linear-gradient(90deg, #4ade80, #22d3ee); transition: width 0.3s; }
.battery-card { opacity: 0.5; }
.battery-card.connected { opacity: 1; }
.battery-card h2::after { content: ' (Disconnected)'; font-size: 0.75rem; color: #f87171; }
.battery-card.connected h2::after { content: ' (Connected)'; color: #4ade80; }
.mode-selector { display: grid; grid-template-columns: repeat(2, 1fr); gap: 0.5rem; margin-bottom: 1rem; }
.mode-selector button { padding: 0.75rem; border: 1px solid #475569; background: transparent; color: #94a3b8; border-radius: 0.375rem; cursor: pointer; transition: all 0.2s; font-size: 0.875rem; }
.mode-selector button:hover { border-color: #38bdf8; color: #f8fafc; }
.mode-selector button.active { background: #38bdf8; color: #0f172a; border-color: #38bdf8; }
.override-control { display: flex; align-items: center; gap: 0.75rem; flex-wrap: wrap; }
.override-control input[type="range"] { flex: 1; min-width: 150px; }
.override-control button { padding: 0.5rem 1rem; border: none; border-radius: 0.375rem; cursor: pointer; font-size: 0.875rem; }
.override-control button:first-of-type { background: #38bdf8; color: #0f172a; }
.override-control button:last-of-type { background: #475569; color: #f8fafc; }
.link-button { display: block; padding: 0.75rem 1rem; background: #334155; color: #f8fafc; text-decoration: none; border-radius: 0.375rem; margin-bottom: 0.5rem; text-align: center; transition: background 0.2s; }
.link-button:hover { background: #475569; }
.timestamp { text-align: center; color: #64748b; font-size: 0.75rem; margin-top: 2rem; }
.init-status { background: #1e3a5f; padding: 1rem 2rem; display: flex; align-items: center; gap: 1rem; border-bottom: 1px solid #334155; }
.init-status.ready { display: none; }
.init-spinner { width: 20px; height: 20px; border: 3px solid #334155; border-top-color: #38bdf8; border-radius: 50%; animation: spin 1s linear infinite; }
@keyframes spin { to { transform: rotate(360deg); } }
#init-message { color: #94a3b8; }
@media (max-width: 600px) {
    .navbar { flex-direction: column; gap: 1rem; }
    .nav-links { display: flex; flex-wrap: wrap; justify-content: center; }
    .nav-links a { margin: 0.25rem; }
    .big { font-size: 2rem; }
}
`
}

func dashboardJS(overrideActive bool) string {
	overrideActiveJS := "false"
	if overrideActive {
		overrideActiveJS = "true"
	}
	return fmt.Sprintf(`
// Override state tracking
var overrideActive = %s;

// Fix navigation links to use port 80 (nginx) instead of current port
// LuCI runs on nginx (port 80), not on energymanager (port 8081)
(function() {
    var baseURL = window.location.protocol + '//' + window.location.hostname;

    // LuCI links (both nav and quick links)
    ['luci-link', 'luci-admin-link'].forEach(function(id) {
        var el = document.getElementById(id);
        if (el) el.href = baseURL + '/cgi-bin/luci/admin/';
    });

    // GL.iNet link (main admin interface)
    var glinetLink = document.getElementById('glinet-link');
    if (glinetLink) glinetLink.href = baseURL + '/';
})();

// Check initialization status
function checkInitStatus() {
    fetch('/api/init')
        .then(r => r.json())
        .then(status => {
            var el = document.getElementById('init-status');
            var msg = document.getElementById('init-message');
            if (status.ready) {
                el.style.display = 'none';
            } else {
                el.style.display = 'flex';
                msg.textContent = status.description || 'Initializing...';
            }
        })
        .catch(() => {});
}
checkInitStatus();
setInterval(checkInitStatus, 2000);

function setMode(mode) {
    fetch('/api/mode', {
        method: 'POST',
        headers: {'Content-Type': 'application/json'},
        body: JSON.stringify({mode: mode})
    }).then(() => location.reload());
}

function updateOverrideDisplay() {
    const slider = document.getElementById('override-slider');
    const display = document.getElementById('override-value');
    display.textContent = slider.value + ' W';
}

function setOverride(enabled) {
    const value = enabled ? parseInt(document.getElementById('override-slider').value) : 0;
    fetch('/api/override', {
        method: 'POST',
        headers: {'Content-Type': 'application/json'},
        body: JSON.stringify({enabled: enabled, value: value})
    }).then(() => location.reload());
}

// Auto-refresh
setInterval(() => {
    fetch('/api/status')
        .then(r => r.json())
        .then(data => {
            if (data.grid) {
                var gridBadge = document.querySelector('.grid-card .status-badge');
                var gridError = document.getElementById('grid-error');
                if (data.grid.valid) {
                    document.getElementById('grid-power').textContent = Math.round(data.grid.total_power_w) + ' W';
                    gridBadge.className = 'status-badge online';
                    gridBadge.textContent = 'Online';
                    gridError.textContent = '';
                } else {
                    document.getElementById('grid-power').textContent = '---';
                    gridBadge.className = 'status-badge ' + (data.grid.error ? 'error' : 'offline');
                    gridBadge.textContent = data.grid.error ? 'Error' : 'Offline';
                    gridError.textContent = data.grid.error || '';
                }
            }
            if (data.inverter && data.inverter.valid) {
                document.getElementById('inv-state').textContent = data.inverter.state;
                document.getElementById('inv-power').textContent = Math.round(data.inverter.bat_power_w) + ' W';
                document.getElementById('bat-voltage').textContent = data.inverter.bat_voltage_v.toFixed(1) + ' V';
                document.getElementById('bat-soc').textContent = Math.round(data.inverter.bat_soc_pct) + '%';
                document.getElementById('soc-fill').style.width = data.inverter.bat_soc_pct + '%';
                if (data.inverter.ac_in_voltage_v > 0) {
                    document.getElementById('inv-ac-in-v').textContent = data.inverter.ac_in_voltage_v.toFixed(1) + ' V';
                    document.getElementById('inv-ac-in-a').textContent = data.inverter.ac_in_current_a.toFixed(1) + ' A';
                }
                if (data.inverter.ac_out_voltage_v > 0) {
                    document.getElementById('inv-ac-out-v').textContent = data.inverter.ac_out_voltage_v.toFixed(1) + ' V';
                    document.getElementById('inv-ac-out-a').textContent = data.inverter.ac_out_current_a.toFixed(1) + ' A';
                }
            }
            if (data.battery && data.battery.valid) {
                document.querySelector('.battery-card').classList.add('connected');
                document.getElementById('bmv-product').textContent = data.battery.product_name || '---';
                document.getElementById('bmv-voltage').textContent = data.battery.voltage_v.toFixed(2) + ' V';
                document.getElementById('bmv-current').textContent = data.battery.current_a.toFixed(2) + ' A';
                document.getElementById('bmv-power').textContent = Math.round(data.battery.power_w) + ' W';
                document.getElementById('bmv-soc').textContent = data.battery.soc_percent.toFixed(1) + '%';
                const ttg = data.battery.time_to_go_min;
                if (ttg > 0) {
                    const h = Math.floor(ttg / 60);
                    const m = ttg % 60;
                    document.getElementById('bmv-ttg').textContent = h + 'h ' + m + 'm';
                } else if (ttg === -1) {
                    document.getElementById('bmv-ttg').textContent = 'Infinite';
                } else {
                    document.getElementById('bmv-ttg').textContent = '---';
                }
                // Alarm
                var alarmEl = document.getElementById('bmv-alarm');
                if (data.battery.alarm_active && data.battery.alarm_reason) {
                    alarmEl.textContent = 'ALARM: ' + data.battery.alarm_reason;
                } else {
                    alarmEl.textContent = '';
                }
                // Energy totals
                document.getElementById('bmv-charged').textContent = data.battery.charged_kwh.toFixed(2) + ' kWh';
                document.getElementById('bmv-discharged').textContent = data.battery.discharged_kwh.toFixed(2) + ' kWh';
                document.getElementById('bmv-consumed').textContent = data.battery.consumed_ah.toFixed(2) + ' Ah';
                document.getElementById('bmv-cycles').textContent = data.battery.charge_cycles || '---';
                // History
                document.getElementById('bmv-min-v').textContent = data.battery.min_voltage_v ? data.battery.min_voltage_v.toFixed(2) + ' V' : '---';
                document.getElementById('bmv-max-v').textContent = data.battery.max_voltage_v ? data.battery.max_voltage_v.toFixed(2) + ' V' : '---';
                if (data.battery.seconds_since_full_charge > 0) {
                    var sh = Math.floor(data.battery.seconds_since_full_charge / 3600);
                    var sm = Math.floor((data.battery.seconds_since_full_charge % 3600) / 60);
                    document.getElementById('bmv-since-full').textContent = sh + 'h ' + sm + 'm';
                } else {
                    document.getElementById('bmv-since-full').textContent = '---';
                }
                if (data.battery.has_temperature) {
                    document.getElementById('bmv-temp').textContent = data.battery.temperature_c.toFixed(1) + 'C';
                } else {
                    document.getElementById('bmv-temp').textContent = '---';
                }
                // Device info
                document.getElementById('bmv-fw').textContent = data.battery.firmware_version || '---';
                document.getElementById('bmv-serial').textContent = data.battery.serial_number || '---';
            } else {
                document.querySelector('.battery-card').classList.remove('connected');
            }
            if (data.pid) {
                document.getElementById('pid-output').textContent = Math.round(data.pid.output_w) + ' W';
            }
            if (data.mode) {
                document.getElementById('mode-name').textContent = data.mode.current;
                document.getElementById('mode-desc').textContent = data.mode.description;
                // Sync override state from server (but don't change slider if user is dragging)
                if (data.mode.override !== overrideActive) {
                    overrideActive = data.mode.override;
                    if (!overrideActive) {
                        // Override was cleared, reset slider to 0
                        document.getElementById('override-slider').value = 0;
                        document.getElementById('override-value').textContent = '0 W';
                    }
                }
                // If override is active, sync the value from server
                if (overrideActive && data.mode.override_value_w !== undefined) {
                    var serverValue = Math.round(data.mode.override_value_w);
                    var slider = document.getElementById('override-slider');
                    // Only update if different (to avoid interrupting user interaction)
                    if (parseInt(slider.value) !== serverValue) {
                        slider.value = serverValue;
                        document.getElementById('override-value').textContent = serverValue + ' W';
                    }
                }
            }
        })
        .catch(() => {});
}, 2000);
`, overrideActiveJS)
}

func (h *Handler) configHTML() string {
	cfg, _ := json.MarshalIndent(h.config, "", "  ")

	return fmt.Sprintf(`<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Configuration - Energy Manager</title>
<style>
%s
.config-section { margin-bottom: 2rem; }
.form-group { margin-bottom: 1rem; }
.form-group label { display: block; color: #94a3b8; margin-bottom: 0.5rem; font-size: 0.875rem; }
.form-group input, .form-group select { width: 100%%; padding: 0.75rem; background: #0f172a; border: 1px solid #475569; border-radius: 0.375rem; color: #f8fafc; font-size: 1rem; }
.form-group input:focus, .form-group select:focus { outline: none; border-color: #38bdf8; }
.checkbox-group { display: flex; align-items: center; gap: 0.75rem; }
.checkbox-group input[type="checkbox"] { width: auto; margin: 0; cursor: pointer; }
.checkbox-group label { display: inline; margin: 0; cursor: pointer; }
.btn { padding: 0.75rem 1.5rem; border: none; border-radius: 0.375rem; cursor: pointer; font-size: 1rem; margin-right: 0.5rem; }
.btn-primary { background: #38bdf8; color: #0f172a; }
.btn-secondary { background: #475569; color: #f8fafc; }
.btn-success { background: #4ade80; color: #0f172a; }
.discovery-results { background: #0f172a; padding: 1rem; border-radius: 0.5rem; margin-top: 1rem; max-height: 200px; overflow-y: auto; }
.device-item { padding: 0.5rem; border-bottom: 1px solid #334155; display: flex; justify-content: space-between; align-items: center; }
.device-item:last-child { border-bottom: none; }
.device-info { font-size: 0.875rem; }
.device-actions button { padding: 0.25rem 0.5rem; font-size: 0.75rem; }
textarea { width: 100%%; min-height: 200px; padding: 1rem; background: #0f172a; border: 1px solid #475569; border-radius: 0.5rem; color: #f8fafc; font-family: monospace; font-size: 0.875rem; }
.tou-period { background: #0f172a; padding: 1rem; border-radius: 0.5rem; margin-bottom: 0.5rem; }
.tou-period .form-row { display: grid; grid-template-columns: repeat(auto-fit, minmax(100px, 1fr)); gap: 0.5rem; }
</style>
</head>
<body>
<nav class="navbar">
    <div class="nav-brand">Energy Manager</div>
    <div class="nav-links">
        <a href="/">Dashboard</a>
        <a href="/config" class="active">Config</a>
        <a href="/metrics">Metrics</a>
        <a id="luci-link" href="/cgi-bin/luci/" target="_blank">LuCI</a>
    </div>
</nav>

<main class="container">
    <h1 style="margin-bottom: 2rem;">Configuration</h1>

    <div class="grid-container">
        <div class="card">
            <h2>Device Discovery</h2>
            <p style="color: #64748b; font-size: 0.875rem; margin-bottom: 1rem;">
                Scan local network for Shelly devices
            </p>
            <button class="btn btn-primary" onclick="discoverDevices()">Scan Network</button>
            <div id="discovery-results" class="discovery-results" style="display:none;"></div>
        </div>

        <div class="card">
            <h2>Shelly Pro EM3</h2>
            <div class="form-group">
                <label>Shelly Address (IP:Port)</label>
                <input type="text" id="shelly-addr" value="%s" placeholder="192.168.254.100:502">
            </div>
            <div class="form-group">
                <label>Poll Interval</label>
                <select id="shelly-interval">
                    <option value="16ms">16ms (60Hz)</option>
                    <option value="20ms">20ms (50Hz)</option>
                    <option value="33ms">33ms (30Hz)</option>
                    <option value="40ms">40ms (25Hz)</option>
                    <option value="50ms">50ms (20Hz)</option>
                    <option value="100ms">100ms (10Hz)</option>
                    <option value="250ms">250ms (4Hz)</option>
                    <option value="500ms">500ms (2Hz)</option>
                    <option value="750ms">750ms</option>
                    <option value="1s" selected>1 second</option>
                    <option value="2s">2 seconds</option>
                    <option value="5s">5 seconds</option>
                </select>
                <small style="color: #64748b; display: block; margin-top: 0.25rem;">
                    Shelly Pro EM3 updates at 50Hz (20ms). Faster polling uses more CPU.
                </small>
            </div>
        </div>

        <div class="card">
            <h2>Victron Inverter (VE.Bus)</h2>
            <div class="form-group">
                <label>Serial Port (MK3 USB)</label>
                <select id="victron-port" class="port-select">
                    <option value="%s" selected>%s</option>
                </select>
            </div>
            <div class="form-group">
                <label>Max Power (W)</label>
                <input type="number" id="max-power" value="%.0f" min="0" max="10000">
            </div>
            <div class="form-group">
                <label>Battery Capacity (kWh)</label>
                <input type="number" id="battery-capacity" value="%.1f" min="0" max="100" step="0.1">
            </div>
        </div>

        <div class="card">
            <h2>Battery Monitor (VE.Direct)</h2>
            <p style="color: #64748b; font-size: 0.875rem; margin-bottom: 1rem;">
                BMV-700/712 or SmartShunt via VE.Direct USB cable
            </p>
            <div class="form-group">
                <label>Serial Port</label>
                <select id="bmv-port" class="port-select">
                    <option value="%s" selected>%s</option>
                </select>
            </div>
            <button type="button" class="btn btn-secondary" onclick="refreshPorts()">Refresh Serial Ports</button>
        </div>

        <div class="card">
            <h2>PID Controller</h2>
            <div class="form-group">
                <label>Proportional Gain (Kp)</label>
                <input type="number" id="pid-kp" value="%.2f" min="0" max="5" step="0.01">
            </div>
            <div class="form-group">
                <label>Integral Gain (Ki)</label>
                <input type="number" id="pid-ki" value="%.3f" min="0" max="1" step="0.001">
            </div>
            <div class="form-group">
                <label>Derivative Gain (Kd)</label>
                <input type="number" id="pid-kd" value="%.2f" min="0" max="1" step="0.01">
            </div>
            <div class="form-group">
                <label>Default Setpoint (W)</label>
                <input type="number" id="pid-setpoint" value="%.0f" min="-1000" max="1000">
            </div>
        </div>

        <div class="card">
            <h2>Location (for sunrise calc)</h2>
            <div class="form-group">
                <label>Latitude</label>
                <input type="number" id="latitude" value="%.4f" step="0.0001">
            </div>
            <div class="form-group">
                <label>Longitude</label>
                <input type="number" id="longitude" value="%.4f" step="0.0001">
            </div>
        </div>

        <div class="card">
            <h2>Solar Mode</h2>
            <div class="form-group checkbox-group">
                <input type="checkbox" id="solar-zero-export" %s>
                <label for="solar-zero-export">Zero Export (never send power to grid)</label>
            </div>
            <div class="form-group" id="export-limit-group">
                <label>Export Limit (W, only if Zero Export disabled)</label>
                <input type="number" id="solar-export-limit" value="%.0f" min="0" max="10000">
            </div>
            <div class="form-group">
                <label>Min SOC (%%)</label>
                <input type="number" id="solar-min-soc" value="%.0f" min="0" max="100">
            </div>
            <div class="form-group">
                <label>Max SOC (%%)</label>
                <input type="number" id="solar-max-soc" value="%.0f" min="0" max="100">
            </div>
            <div class="form-group checkbox-group">
                <input type="checkbox" id="solar-self-use" %s>
                <label for="solar-self-use">Prefer Self-Use (battery for loads when solar insufficient)</label>
            </div>
        </div>

        <div class="card">
            <h2>Battery Save Mode</h2>
            <div class="form-group">
                <label>Reserve SOC (%%)</label>
                <input type="number" id="batsave-min-soc" value="%.0f" min="0" max="100">
            </div>
            <div class="form-group">
                <label>Target SOC at Sunrise (%%)</label>
                <input type="number" id="batsave-target-soc" value="%.0f" min="0" max="100">
            </div>
            <div class="form-group">
                <label>Minutes after sunrise to end</label>
                <input type="number" id="batsave-offset" value="%d" min="0" max="180">
            </div>
        </div>

        <div class="card">
            <h2>MQTT Publisher</h2>
            <div class="form-group checkbox-group">
                <input type="checkbox" id="mqtt-enabled" %s>
                <label for="mqtt-enabled">Enable MQTT Publishing</label>
            </div>
            <div class="form-group">
                <label>Broker Address</label>
                <input type="text" id="mqtt-broker" value="%s" placeholder="tcp://localhost:1883">
            </div>
            <div class="form-group">
                <label>Topic</label>
                <input type="text" id="mqtt-topic" value="%s" placeholder="energy/status">
            </div>
            <div class="form-group">
                <label>Client ID</label>
                <input type="text" id="mqtt-client-id" value="%s" placeholder="energymanager">
            </div>
            <div class="form-group">
                <label>Username (optional)</label>
                <input type="text" id="mqtt-username" value="%s">
            </div>
            <div class="form-group">
                <label>Password (optional)</label>
                <input type="password" id="mqtt-password" value="%s">
            </div>
        </div>

        <div class="card">
            <h2>InfluxDB Time Series</h2>
            <p style="color: #64748b; font-size: 0.875rem; margin-bottom: 1rem;">
                Export metrics to InfluxDB for long-term storage and Grafana dashboards
            </p>
            <div class="form-group checkbox-group">
                <input type="checkbox" id="influx-enabled" %s>
                <label for="influx-enabled">Enable InfluxDB Export</label>
            </div>
            <div class="form-group">
                <label>Server URL</label>
                <input type="text" id="influx-url" value="%s" placeholder="http://localhost:8086">
            </div>
            <div class="form-group">
                <label>Database/Bucket Name</label>
                <input type="text" id="influx-database" value="%s" placeholder="energy">
            </div>
            <div class="form-group">
                <label>Batch Interval</label>
                <select id="influx-interval">
                    <option value="1m" %s>1 minute</option>
                    <option value="5m" %s>5 minutes</option>
                    <option value="10m" %s>10 minutes</option>
                    <option value="15m" %s>15 minutes</option>
                </select>
            </div>
            <hr style="border-color: #334155; margin: 1rem 0;">
            <p style="color: #64748b; font-size: 0.75rem; margin-bottom: 0.5rem;">InfluxDB 1.x Authentication</p>
            <div class="form-group">
                <label>Username (optional)</label>
                <input type="text" id="influx-username" value="%s">
            </div>
            <div class="form-group">
                <label>Password (optional)</label>
                <input type="password" id="influx-password" value="%s">
            </div>
            <hr style="border-color: #334155; margin: 1rem 0;">
            <p style="color: #64748b; font-size: 0.75rem; margin-bottom: 0.5rem;">InfluxDB 2.x Authentication</p>
            <div class="form-group">
                <label>Organization</label>
                <input type="text" id="influx-org" value="%s" placeholder="my-org">
            </div>
            <div class="form-group">
                <label>API Token</label>
                <input type="password" id="influx-token" value="%s">
            </div>
        </div>

        <div class="card" style="grid-column: 1 / -1;">
            <h2>Time-of-Use Schedule</h2>
            <p style="color: #64748b; font-size: 0.875rem; margin-bottom: 1rem;">
                Define pricing periods and actions (charge/discharge/hold)
            </p>
            <div id="tou-periods"></div>
            <button class="btn btn-secondary" onclick="addTOUPeriod()">+ Add Period</button>
        </div>

        <div class="card" style="grid-column: 1 / -1;">
            <h2>Mode Schedule</h2>
            <p style="color: #64748b; font-size: 0.875rem; margin-bottom: 1rem;">
                Automate mode switching based on time (supports sunrise/sunset-relative times).
                Enable "Scheduled" mode on dashboard to activate.
            </p>
            <div class="form-group checkbox-group">
                <input type="checkbox" id="schedule-enabled" %s>
                <label for="schedule-enabled">Enable Mode Schedule</label>
            </div>
            <div id="schedule-periods"></div>
            <button class="btn btn-secondary" onclick="addSchedulePeriod()">+ Add Schedule Period</button>

            <div style="margin-top: 1rem; padding: 1rem; background: #0f172a; border-radius: 0.5rem;">
                <h3 style="margin-bottom: 0.5rem;">Quick Setup Examples</h3>
                <button class="btn btn-secondary" onclick="loadExampleSchedule('default')" style="margin-right: 0.5rem; margin-bottom: 0.5rem;">
                    Standard (BatSave→Solar→TOU)
                </button>
                <button class="btn btn-secondary" onclick="loadExampleSchedule('solar_priority')" style="margin-right: 0.5rem; margin-bottom: 0.5rem;">
                    Solar Priority
                </button>
                <button class="btn btn-secondary" onclick="loadExampleSchedule('tou_only')" style="margin-bottom: 0.5rem;">
                    TOU Only
                </button>
            </div>
        </div>
    </div>

    <div style="margin-top: 2rem; text-align: center;">
        <button class="btn btn-primary" onclick="saveConfig()">Save Configuration</button>
        <button class="btn btn-secondary" onclick="location.href='/'">Cancel</button>
    </div>

    <div class="card" style="margin-top: 2rem;">
        <h2>Raw Configuration (JSON)</h2>
        <textarea id="raw-config">%s</textarea>
        <button class="btn btn-secondary" style="margin-top: 1rem;" onclick="loadRawConfig()">Load from JSON</button>
    </div>
</main>

<script>
// Fix LuCI links to use port 80 (nginx) instead of current port
// LuCI runs on nginx (port 80), not on energymanager (port 8081)
(function() {
    var luciBase = window.location.protocol + '//' + window.location.hostname;
    var luciLinks = document.querySelectorAll('#luci-link, #luci-admin-link');
    luciLinks.forEach(function(link) {
        link.href = luciBase + '/cgi-bin/luci/admin/';
    });
})();

let touPeriods = %s;

function renderTOUPeriods() {
    const container = document.getElementById('tou-periods');
    container.innerHTML = touPeriods.map((p, i) => ` + "`" + `
        <div class="tou-period">
            <div class="form-row">
                <div class="form-group">
                    <label>Name</label>
                    <input type="text" value="${p.name}" onchange="touPeriods[${i}].name=this.value">
                </div>
                <div class="form-group">
                    <label>Start Hour</label>
                    <input type="number" value="${p.start_hour}" min="0" max="23" onchange="touPeriods[${i}].start_hour=parseInt(this.value)">
                </div>
                <div class="form-group">
                    <label>End Hour</label>
                    <input type="number" value="${p.end_hour}" min="0" max="24" onchange="touPeriods[${i}].end_hour=parseInt(this.value)">
                </div>
                <div class="form-group">
                    <label>Rate ($/kWh)</label>
                    <input type="number" value="${p.rate}" step="0.01" onchange="touPeriods[${i}].rate=parseFloat(this.value)">
                </div>
                <div class="form-group">
                    <label>Action</label>
                    <select onchange="touPeriods[${i}].action=this.value">
                        <option value="charge" ${p.action==='charge'?'selected':''}>Charge</option>
                        <option value="discharge" ${p.action==='discharge'?'selected':''}>Discharge</option>
                        <option value="hold" ${p.action==='hold'?'selected':''}>Hold</option>
                    </select>
                </div>
                <div class="form-group">
                    <label>&nbsp;</label>
                    <button class="btn btn-secondary" onclick="touPeriods.splice(${i},1);renderTOUPeriods()">Remove</button>
                </div>
            </div>
        </div>
    ` + "`" + `).join('');
}

function addTOUPeriod() {
    touPeriods.push({name: 'new', start_hour: 0, end_hour: 6, days: [0,1,2,3,4,5,6], rate: 0.10, action: 'charge'});
    renderTOUPeriods();
}

function discoverDevices() {
    const results = document.getElementById('discovery-results');
    results.style.display = 'block';
    results.innerHTML = '<p>Scanning...</p>';

    fetch('/api/discover')
        .then(r => r.json())
        .then(data => {
            if (data.error) {
                results.innerHTML = '<p style="color:#f87171;">Error: ' + data.error + '</p>';
                return;
            }
            const devices = data.em3_devices || [];
            if (devices.length === 0) {
                results.innerHTML = '<p>No Shelly EM3 devices found. Found ' + (data.all_devices?.length || 0) + ' other Shelly devices.</p>';
                return;
            }
            results.innerHTML = devices.map(d => ` + "`" + `
                <div class="device-item">
                    <div class="device-info">
                        <strong>${d.model}</strong> - ${d.ip}
                        ${d.modbus ? '<span style="color:#4ade80;"> (Modbus OK)</span>' : '<span style="color:#facc15;"> (Modbus disabled)</span>'}
                    </div>
                    <div class="device-actions">
                        <button class="btn btn-primary" onclick="useDevice('${d.modbus_ip || d.ip + ':502'}')">Use</button>
                    </div>
                </div>
            ` + "`" + `).join('');
        })
        .catch(err => {
            results.innerHTML = '<p style="color:#f87171;">Error: ' + err + '</p>';
        });
}

function useDevice(addr) {
    document.getElementById('shelly-addr').value = addr;
}

// Parse interval string to nanoseconds for Go time.Duration
function parseInfluxInterval(str) {
    const val = parseInt(str);
    if (str.endsWith('m')) return val * 60 * 1000000000;
    if (str.endsWith('h')) return val * 60 * 60 * 1000000000;
    return 5 * 60 * 1000000000; // default 5 minutes
}

function saveConfig() {
    const config = {
        shelly_addr: document.getElementById('shelly-addr').value,
        victron_port: document.getElementById('victron-port').value,
        bmv_port: document.getElementById('bmv-port').value,
        max_power: parseFloat(document.getElementById('max-power').value),
        battery_capacity: parseFloat(document.getElementById('battery-capacity').value),
        latitude: parseFloat(document.getElementById('latitude').value),
        longitude: parseFloat(document.getElementById('longitude').value),
        pid_kp: parseFloat(document.getElementById('pid-kp').value),
        pid_ki: parseFloat(document.getElementById('pid-ki').value),
        pid_kd: parseFloat(document.getElementById('pid-kd').value),
        pid_setpoint: parseFloat(document.getElementById('pid-setpoint').value),
        tou_schedule: { periods: touPeriods },
        mode_schedule: { enabled: document.getElementById('schedule-enabled').checked, periods: schedulePeriods },
        solar_config: {
            zero_export: document.getElementById('solar-zero-export').checked,
            export_limit_w: parseFloat(document.getElementById('solar-export-limit').value),
            min_soc_pct: parseFloat(document.getElementById('solar-min-soc').value),
            max_soc_pct: parseFloat(document.getElementById('solar-max-soc').value),
            prefer_self_use: document.getElementById('solar-self-use').checked,
        },
        battery_save_config: {
            min_soc_pct: parseFloat(document.getElementById('batsave-min-soc').value),
            target_soc_at_sunrise: parseFloat(document.getElementById('batsave-target-soc').value),
            sunrise_offset_min: parseInt(document.getElementById('batsave-offset').value),
        },
        mqtt_enabled: document.getElementById('mqtt-enabled').checked,
        mqtt_broker: document.getElementById('mqtt-broker').value,
        mqtt_topic: document.getElementById('mqtt-topic').value,
        mqtt_client_id: document.getElementById('mqtt-client-id').value,
        mqtt_username: document.getElementById('mqtt-username').value,
        mqtt_password: document.getElementById('mqtt-password').value,
        influx_enabled: document.getElementById('influx-enabled').checked,
        influx_url: document.getElementById('influx-url').value,
        influx_database: document.getElementById('influx-database').value,
        influx_interval: parseInfluxInterval(document.getElementById('influx-interval').value),
        influx_username: document.getElementById('influx-username').value,
        influx_password: document.getElementById('influx-password').value,
        influx_org: document.getElementById('influx-org').value,
        influx_token: document.getElementById('influx-token').value
    };

    fetch('/api/config', {
        method: 'POST',
        headers: {'Content-Type': 'application/json'},
        body: JSON.stringify(config)
    })
    .then(r => r.json())
    .then(data => {
        if (data.status === 'ok') {
            // Show success message without redirect
            const btn = document.querySelector('.btn-primary');
            const origText = btn.textContent;
            btn.textContent = 'Saved!';
            btn.style.background = '#4ade80';
            setTimeout(() => {
                btn.textContent = origText;
                btn.style.background = '';
            }, 2000);
        } else {
            alert('Error saving configuration');
        }
    })
    .catch(err => alert('Error: ' + err));
}

function loadRawConfig() {
    try {
        const config = JSON.parse(document.getElementById('raw-config').value);
        fetch('/api/config', {
            method: 'POST',
            headers: {'Content-Type': 'application/json'},
            body: JSON.stringify(config)
        })
        .then(() => location.reload());
    } catch (e) {
        alert('Invalid JSON: ' + e);
    }
}

// Schedule periods management
let schedulePeriods = %s;

function renderSchedulePeriods() {
    const container = document.getElementById('schedule-periods');
    container.innerHTML = schedulePeriods.map((p, i) => ` + "`" + `
        <div class="tou-period">
            <div class="form-row">
                <div class="form-group">
                    <label>Name</label>
                    <input type="text" value="${p.name}" onchange="schedulePeriods[${i}].name=this.value">
                </div>
                <div class="form-group">
                    <label>Start Time Type</label>
                    <select onchange="schedulePeriods[${i}].start_time.type=this.value;renderSchedulePeriods()">
                        <option value="absolute" ${p.start_time.type==='absolute'?'selected':''}>Absolute</option>
                        <option value="sunrise" ${p.start_time.type==='sunrise'?'selected':''}>Sunrise</option>
                        <option value="sunset" ${p.start_time.type==='sunset'?'selected':''}>Sunset</option>
                    </select>
                </div>
                ${p.start_time.type === 'absolute' ? ` + "`" + `
                <div class="form-group">
                    <label>Start Hour</label>
                    <input type="number" value="${p.start_time.hour}" min="0" max="23" onchange="schedulePeriods[${i}].start_time.hour=parseInt(this.value)">
                </div>
                ` + "`" + ` : ` + "`" + `
                <div class="form-group">
                    <label>Offset (min)</label>
                    <input type="number" value="${p.start_time.offset||0}" min="-120" max="120" onchange="schedulePeriods[${i}].start_time.offset=parseInt(this.value)">
                </div>
                ` + "`" + `}
                <div class="form-group">
                    <label>End Time Type</label>
                    <select onchange="schedulePeriods[${i}].end_time.type=this.value;renderSchedulePeriods()">
                        <option value="absolute" ${p.end_time.type==='absolute'?'selected':''}>Absolute</option>
                        <option value="sunrise" ${p.end_time.type==='sunrise'?'selected':''}>Sunrise</option>
                        <option value="sunset" ${p.end_time.type==='sunset'?'selected':''}>Sunset</option>
                    </select>
                </div>
                ${p.end_time.type === 'absolute' ? ` + "`" + `
                <div class="form-group">
                    <label>End Hour</label>
                    <input type="number" value="${p.end_time.hour}" min="0" max="24" onchange="schedulePeriods[${i}].end_time.hour=parseInt(this.value)">
                </div>
                ` + "`" + ` : ` + "`" + `
                <div class="form-group">
                    <label>Offset (min)</label>
                    <input type="number" value="${p.end_time.offset||0}" min="-120" max="120" onchange="schedulePeriods[${i}].end_time.offset=parseInt(this.value)">
                </div>
                ` + "`" + `}
                <div class="form-group">
                    <label>Mode</label>
                    <select onchange="schedulePeriods[${i}].mode=this.value">
                        <option value="pid" ${p.mode==='pid'?'selected':''}>PID (Zero Grid)</option>
                        <option value="solar" ${p.mode==='solar'?'selected':''}>Solar</option>
                        <option value="battery_save" ${p.mode==='battery_save'?'selected':''}>Battery Save</option>
                        <option value="tou" ${p.mode==='tou'?'selected':''}>Time of Use</option>
                    </select>
                </div>
                <div class="form-group">
                    <label>Priority</label>
                    <input type="number" value="${p.priority||0}" min="0" max="10" onchange="schedulePeriods[${i}].priority=parseInt(this.value)">
                </div>
                <div class="form-group">
                    <label>&nbsp;</label>
                    <button class="btn btn-secondary" onclick="schedulePeriods.splice(${i},1);renderSchedulePeriods()">Remove</button>
                </div>
            </div>
        </div>
    ` + "`" + `).join('');
}

function addSchedulePeriod() {
    schedulePeriods.push({
        name: 'new period',
        start_time: {type: 'absolute', hour: 0, minute: 0, offset: 0},
        end_time: {type: 'absolute', hour: 6, minute: 0, offset: 0},
        days: [],
        mode: 'pid',
        action: '',
        rate: 0,
        priority: 0
    });
    renderSchedulePeriods();
}

function loadExampleSchedule(example) {
    if (example === 'default') {
        // Midnight-Sunrise: Battery Save, Sunrise-3PM: Solar, 3PM-Midnight: TOU
        schedulePeriods = [
            {name: 'Night (Battery Save)', start_time: {type: 'absolute', hour: 0}, end_time: {type: 'sunrise', offset: 0}, days: [], mode: 'battery_save', priority: 1},
            {name: 'Day (Solar)', start_time: {type: 'sunrise', offset: 0}, end_time: {type: 'absolute', hour: 15}, days: [], mode: 'solar', priority: 1},
            {name: 'Evening (TOU)', start_time: {type: 'absolute', hour: 15}, end_time: {type: 'absolute', hour: 24}, days: [], mode: 'tou', priority: 1}
        ];
    } else if (example === 'solar_priority') {
        // Maximize solar usage all day
        schedulePeriods = [
            {name: 'Night', start_time: {type: 'sunset', offset: 0}, end_time: {type: 'sunrise', offset: 0}, days: [], mode: 'pid', priority: 1},
            {name: 'Day (Solar)', start_time: {type: 'sunrise', offset: 0}, end_time: {type: 'sunset', offset: 0}, days: [], mode: 'solar', priority: 1}
        ];
    } else if (example === 'tou_only') {
        // Simple TOU schedule
        schedulePeriods = [
            {name: 'Off-Peak', start_time: {type: 'absolute', hour: 0}, end_time: {type: 'absolute', hour: 7}, days: [], mode: 'tou', priority: 1},
            {name: 'Peak', start_time: {type: 'absolute', hour: 17}, end_time: {type: 'absolute', hour: 21}, days: [], mode: 'tou', priority: 2},
            {name: 'Shoulder', start_time: {type: 'absolute', hour: 7}, end_time: {type: 'absolute', hour: 17}, days: [], mode: 'pid', priority: 1}
        ];
    }
    document.getElementById('schedule-enabled').checked = true;
    renderSchedulePeriods();
}

// Refresh serial ports from system
function refreshPorts() {
    fetch('/api/ports')
        .then(r => r.json())
        .then(data => {
            const ports = data.ports || [];
            document.querySelectorAll('.port-select').forEach(select => {
                const currentVal = select.value;
                // Keep first option as current value, add detected ports
                const options = ['<option value="' + currentVal + '">' + currentVal + '</option>'];
                ports.forEach(port => {
                    if (port !== currentVal) {
                        options.push('<option value="' + port + '">' + port + '</option>');
                    }
                });
                select.innerHTML = options.join('');
            });
        })
        .catch(err => console.error('Failed to refresh ports:', err));
}

renderTOUPeriods();
renderSchedulePeriods();
refreshPorts(); // Load ports on page load
</script>
</body>
</html>`,
		dashboardCSS(),
		h.config.ShellyAddr,
		h.config.VictronPort, h.config.VictronPort, // port select value and label
		h.config.MaxPower,
		h.config.BatteryCapacity,
		h.config.BMVPort, h.config.BMVPort, // BMV port select value and label
		h.config.PIDKp, h.config.PIDKi, h.config.PIDKd, h.config.PIDSetpoint,
		h.config.Latitude, h.config.Longitude,
		checkedAttr(h.config.SolarConfig.ZeroExport),
		h.config.SolarConfig.ExportLimit,
		h.config.SolarConfig.MinSOC,
		h.config.SolarConfig.MaxSOC,
		checkedAttr(h.config.SolarConfig.PreferSelfUse),
		h.config.BatterySaveConfig.MinSOC,
		h.config.BatterySaveConfig.TargetSOC,
		h.config.BatterySaveConfig.SunriseOffset,
		checkedAttr(h.config.MQTTEnabled),
		h.config.MQTTBroker,
		h.config.MQTTTopic,
		h.config.MQTTClientID,
		h.config.MQTTUsername,
		h.config.MQTTPassword,
		checkedAttr(h.config.InfluxEnabled),
		h.config.InfluxURL,
		h.config.InfluxDatabase,
		h.influxIntervalSelected(1*time.Minute),
		h.influxIntervalSelected(5*time.Minute),
		h.influxIntervalSelected(10*time.Minute),
		h.influxIntervalSelected(15*time.Minute),
		h.config.InfluxUsername,
		h.config.InfluxPassword,
		h.config.InfluxOrg,
		h.config.InfluxToken,
		checkedAttr(h.config.ModeSchedule.Enabled),
		string(cfg),
		h.touPeriodsJSON(),
		h.schedulePeriodsJSON(),
	)
}

func checkedAttr(b bool) string {
	if b {
		return "checked"
	}
	return ""
}

func (h *Handler) influxIntervalSelected(interval time.Duration) string {
	if h.config.InfluxInterval == interval {
		return "selected"
	}
	return ""
}

func (h *Handler) touPeriodsJSON() string {
	if h.config.TOUSchedule.Periods == nil {
		return "[]"
	}
	data, _ := json.Marshal(h.config.TOUSchedule.Periods)
	return string(data)
}

func (h *Handler) schedulePeriodsJSON() string {
	if h.config.ModeSchedule.Periods == nil {
		return "[]"
	}
	data, _ := json.Marshal(h.config.ModeSchedule.Periods)
	return string(data)
}
