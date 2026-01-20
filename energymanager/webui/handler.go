package webui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/diebietse/invertergui/energymanager/auth"
	"github.com/diebietse/invertergui/energymanager/core"
	"github.com/diebietse/invertergui/energymanager/discovery"
	"github.com/diebietse/invertergui/energymanager/modes"
)


// Handler serves the web UI
type Handler struct {
	hub       *core.Hub
	modeCtrl  *modes.Controller
	config    *Config
	auth      *auth.Authenticator
	mu        sync.RWMutex
	latest    *core.EnergyInfo
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
}

// New creates a new web UI handler
func New(hub *core.Hub, modeCtrl *modes.Controller, cfg *Config) *Handler {
	h := &Handler{
		hub:      hub,
		modeCtrl: modeCtrl,
		config:   cfg,
		auth:     auth.New(cfg.Username, cfg.Password), // Fallback to config if system auth fails
	}
	go h.updateLoop()
	return h
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
		h.config = &cfg
		// Apply config to mode controller
		if h.modeCtrl != nil {
			h.modeCtrl.SetTOUSchedule(cfg.TOUSchedule)
			h.modeCtrl.SetSolarConfig(cfg.SolarConfig)
			h.modeCtrl.SetBatterySaveConfig(cfg.BatterySaveConfig)
			h.modeCtrl.SetModeSchedule(cfg.ModeSchedule)
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

func (h *Handler) dashboardHTML(info *core.EnergyInfo) string {
	gridPower := "---"
	gridClass := ""
	phaseHTML := ""
	invState := "---"
	invPower := "---"
	batVoltage := "---"
	batSOC := "---"
	batCurrent := "---"
	batPower := "---"
	batChargedKWh := "---"
	batDischargedKWh := "---"
	batTimeToGo := "---"
	batProduct := "---"
	bmvConnected := false
	modeStr := "---"
	modeDesc := "---"
	pidOutput := "---"
	timestamp := "---"

	if info != nil {
		timestamp = info.Timestamp.Format(time.RFC1123)

		if info.Grid.Valid {
			gridPower = fmt.Sprintf("%.0f W", info.Grid.TotalPower)
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
					<div>%.1f V</div>
					<div>%.2f A</div>
					<div>%.0f W</div>
				</div>`, 'A'+i, p.Voltage, p.Current, p.ActivePower)
			}
		}

		if info.Inverter.Valid {
			invState = info.Inverter.State
			invPower = fmt.Sprintf("%.0f W", info.Inverter.BatPower)
			batVoltage = fmt.Sprintf("%.1f V", info.Inverter.BatVoltage)
			batSOC = fmt.Sprintf("%.0f%%", info.Inverter.BatSOC)
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
			if info.Battery.TimeToGoMin > 0 {
				hours := info.Battery.TimeToGoMin / 60
				mins := info.Battery.TimeToGoMin % 60
				batTimeToGo = fmt.Sprintf("%dh %dm", hours, mins)
			} else if info.Battery.TimeToGoMin == -1 {
				batTimeToGo = "Infinite"
			}
			batProduct = info.Battery.ProductName
		}

		modeStr = string(info.Mode.Current)
		modeDesc = info.Mode.Description
		pidOutput = fmt.Sprintf("%.0f W", info.PID.Output)
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
    </div>
</nav>

<main class="container">
    <div class="grid-container">
        <div class="card grid-card">
            <h2>Grid Power</h2>
            <div class="big %s" id="grid-power">%s</div>
            <div class="phases">%s</div>
        </div>

        <div class="card inverter-card">
            <h2>Inverter</h2>
            <div class="stat-row"><span>State:</span><span id="inv-state">%s</span></div>
            <div class="stat-row"><span>Power:</span><span id="inv-power">%s</span></div>
            <div class="stat-row"><span>Battery:</span><span id="bat-voltage">%s</span></div>
            <div class="stat-row"><span>SOC:</span><span id="bat-soc">%s</span></div>
            <div class="soc-bar"><div class="soc-fill" id="soc-fill" style="width:%s"></div></div>
        </div>

        <div class="card battery-card %s">
            <h2>Battery Monitor (BMV)</h2>
            <div class="stat-row"><span>Device:</span><span id="bmv-product">%s</span></div>
            <div class="stat-row"><span>Voltage:</span><span id="bmv-voltage">%s</span></div>
            <div class="stat-row"><span>Current:</span><span id="bmv-current">%s</span></div>
            <div class="stat-row"><span>Power:</span><span id="bmv-power">%s</span></div>
            <div class="stat-row"><span>SOC:</span><span id="bmv-soc">%s</span></div>
            <div class="stat-row"><span>Time to Go:</span><span id="bmv-ttg">%s</span></div>
            <div class="stat-row"><span>Charged:</span><span id="bmv-charged">%s</span></div>
            <div class="stat-row"><span>Discharged:</span><span id="bmv-discharged">%s</span></div>
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
                <input type="range" id="override-slider" min="-5000" max="5000" value="0" oninput="updateOverrideDisplay()">
                <span id="override-value">0 W</span>
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
		gridClass, gridPower, phaseHTML,
		invState, invPower, batVoltage, batSOC, batSOC,
		bmvClass, batProduct, batVoltage, batCurrent, batPower, batSOC, batTimeToGo, batChargedKWh, batDischargedKWh,
		modeButtonClass("scheduled", modeStr),
		modeButtonClass("pid", modeStr),
		modeButtonClass("tou", modeStr),
		modeButtonClass("solar", modeStr),
		modeButtonClass("battery_save", modeStr),
		modeStr, modeDesc, pidOutput,
		timestamp,
		dashboardJS(),
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
@media (max-width: 600px) {
    .navbar { flex-direction: column; gap: 1rem; }
    .nav-links { display: flex; flex-wrap: wrap; justify-content: center; }
    .nav-links a { margin: 0.25rem; }
    .big { font-size: 2rem; }
}
`
}

func dashboardJS() string {
	return `
// Fix LuCI links to use port 80 (nginx) instead of current port
(function() {
    var luciBase = window.location.protocol + '//' + window.location.hostname;
    var luciLinks = document.querySelectorAll('#luci-link, #luci-admin-link');
    luciLinks.forEach(function(link) {
        link.href = luciBase + '/cgi-bin/luci/';
    });
})();

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
            if (data.grid && data.grid.valid) {
                document.getElementById('grid-power').textContent = Math.round(data.grid.total_power_w) + ' W';
            }
            if (data.inverter && data.inverter.valid) {
                document.getElementById('inv-state').textContent = data.inverter.state;
                document.getElementById('inv-power').textContent = Math.round(data.inverter.bat_power_w) + ' W';
                document.getElementById('bat-voltage').textContent = data.inverter.bat_voltage_v.toFixed(1) + ' V';
                document.getElementById('bat-soc').textContent = Math.round(data.inverter.bat_soc_pct) + '%';
                document.getElementById('soc-fill').style.width = data.inverter.bat_soc_pct + '%';
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
                document.getElementById('bmv-charged').textContent = data.battery.charged_kwh.toFixed(2) + ' kWh';
                document.getElementById('bmv-discharged').textContent = data.battery.discharged_kwh.toFixed(2) + ' kWh';
            } else {
                document.querySelector('.battery-card').classList.remove('connected');
            }
            if (data.pid) {
                document.getElementById('pid-output').textContent = Math.round(data.pid.output_w) + ' W';
            }
            if (data.mode) {
                document.getElementById('mode-name').textContent = data.mode.current;
                document.getElementById('mode-desc').textContent = data.mode.description;
            }
        })
        .catch(() => {});
}, 2000);
`
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
(function() {
    var luciBase = window.location.protocol + '//' + window.location.hostname;
    var luciLinks = document.querySelectorAll('#luci-link, #luci-admin-link');
    luciLinks.forEach(function(link) {
        link.href = luciBase + '/cgi-bin/luci/';
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
        }
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
