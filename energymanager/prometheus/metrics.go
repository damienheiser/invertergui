package prometheus

import (
	"net/http"
	"sync"

	"github.com/diebietse/invertergui/energymanager/core"
)

// Metrics serves Prometheus metrics
type Metrics struct {
	hub    *core.Hub
	mu     sync.RWMutex
	latest *core.EnergyInfo
}

// New creates a new Prometheus metrics handler
func New(hub *core.Hub) *Metrics {
	m := &Metrics{hub: hub}
	go m.updateLoop()
	return m
}

func (m *Metrics) updateLoop() {
	sub := m.hub.Subscribe()
	for info := range sub.C() {
		m.mu.Lock()
		m.latest = info
		m.mu.Unlock()
	}
}

// ServeHTTP serves Prometheus metrics
func (m *Metrics) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.RLock()
	info := m.latest
	m.mu.RUnlock()

	w.Header().Set("Content-Type", "text/plain; version=0.0.4")

	if info == nil {
		w.Write([]byte("# No data available\n"))
		return
	}

	// Grid metrics
	if info.Grid.Valid {
		writeMetric(w, "grid_power_watts", info.Grid.TotalPower, "Total grid power (positive=import)")
		writeMetric(w, "grid_current_amps", info.Grid.TotalCurrent, "Total grid current")
		writeMetric(w, "grid_apparent_va", info.Grid.TotalApparent, "Total apparent power")

		for i, p := range info.Grid.Phases {
			phase := string(rune('a' + i))
			writeMetricLabeled(w, "grid_phase_voltage_volts", p.Voltage, "phase", phase, "Phase voltage")
			writeMetricLabeled(w, "grid_phase_current_amps", p.Current, "phase", phase, "Phase current")
			writeMetricLabeled(w, "grid_phase_power_watts", p.ActivePower, "phase", phase, "Phase active power")
			writeMetricLabeled(w, "grid_phase_apparent_va", p.ApparentPower, "phase", phase, "Phase apparent power")
			writeMetricLabeled(w, "grid_phase_power_factor", p.PowerFactor, "phase", phase, "Phase power factor")
			writeMetricLabeled(w, "grid_phase_frequency_hz", p.Frequency, "phase", phase, "Phase frequency")
		}
	}

	// Inverter metrics
	if info.Inverter.Valid {
		writeMetric(w, "inverter_battery_voltage_volts", info.Inverter.BatVoltage, "Battery voltage")
		writeMetric(w, "inverter_battery_current_amps", info.Inverter.BatCurrent, "Battery current (positive=charging)")
		writeMetric(w, "inverter_battery_power_watts", info.Inverter.BatPower, "Battery power")
		writeMetric(w, "inverter_battery_soc_percent", info.Inverter.BatSOC, "Battery state of charge")
		writeMetric(w, "inverter_ac_in_voltage_volts", info.Inverter.ACInVoltage, "AC input voltage")
		writeMetric(w, "inverter_ac_in_current_amps", info.Inverter.ACInCurrent, "AC input current")
		writeMetric(w, "inverter_ac_out_voltage_volts", info.Inverter.ACOutVoltage, "AC output voltage")
		writeMetric(w, "inverter_ac_out_current_amps", info.Inverter.ACOutCurrent, "AC output current")
		writeMetricLabeled(w, "inverter_state", 1, "state", info.Inverter.State, "Inverter state")
	}

	// Battery monitor metrics (BMV via VE.Direct)
	if info.Battery.Valid {
		writeMetric(w, "battery_voltage_volts", info.Battery.VoltageV, "Battery voltage from BMV")
		writeMetric(w, "battery_current_amps", info.Battery.CurrentA, "Battery current (negative=discharging)")
		writeMetric(w, "battery_power_watts", info.Battery.PowerW, "Battery instantaneous power")
		writeMetric(w, "battery_soc_percent", info.Battery.SOCPercent, "Battery state of charge")
		writeMetric(w, "battery_consumed_ah", info.Battery.ConsumedAh, "Consumed Ah since full charge")
		writeMetric(w, "battery_time_to_go_minutes", float64(info.Battery.TimeToGoMin), "Minutes until empty (-1=infinite)")
		writeMetric(w, "battery_charged_kwh_total", info.Battery.ChargedKWh, "Total energy charged")
		writeMetric(w, "battery_discharged_kwh_total", info.Battery.DischargedKWh, "Total energy discharged")
		writeMetric(w, "battery_charge_cycles_total", float64(info.Battery.ChargeCycles), "Number of charge cycles")
		writeMetric(w, "battery_full_discharges_total", float64(info.Battery.FullDischarges), "Number of full discharges")
		writeMetric(w, "battery_min_voltage_volts", info.Battery.MinVoltageV, "Minimum recorded voltage")
		writeMetric(w, "battery_max_voltage_volts", info.Battery.MaxVoltageV, "Maximum recorded voltage")
		writeMetric(w, "battery_deepest_discharge_ah", info.Battery.DeepestDischargeAh, "Deepest discharge")
		writeMetric(w, "battery_seconds_since_full_charge", float64(info.Battery.SecondsSinceFullCharge), "Seconds since last full charge")
		if info.Battery.HasTemperature {
			writeMetric(w, "battery_temperature_celsius", info.Battery.TemperatureC, "Battery temperature")
		}
		if info.Battery.AlarmActive {
			writeMetricLabeled(w, "battery_alarm_active", 1, "reason", info.Battery.AlarmReason, "Battery alarm active")
		} else {
			writeMetric(w, "battery_alarm_active", 0, "Battery alarm active")
		}
		writeMetric(w, "battery_low_voltage_alarms_total", float64(info.Battery.LowVoltageAlarms), "Low voltage alarm count")
		writeMetric(w, "battery_high_voltage_alarms_total", float64(info.Battery.HighVoltageAlarms), "High voltage alarm count")
	}

	// PID metrics
	writeMetric(w, "pid_setpoint_watts", info.PID.Setpoint, "PID setpoint (target grid power)")
	writeMetric(w, "pid_output_watts", info.PID.Output, "PID output (inverter command)")
	writeMetric(w, "pid_error_watts", info.PID.Error, "PID error (setpoint - grid)")
	if info.PID.Enabled {
		writeMetric(w, "pid_enabled", 1, "PID controller enabled")
	} else {
		writeMetric(w, "pid_enabled", 0, "PID controller enabled")
	}
}

func writeMetric(w http.ResponseWriter, name string, value float64, help string) {
	w.Write([]byte("# HELP " + name + " " + help + "\n"))
	w.Write([]byte("# TYPE " + name + " gauge\n"))
	w.Write([]byte(name + " " + formatFloat(value) + "\n"))
}

func writeMetricLabeled(w http.ResponseWriter, name string, value float64, labelKey, labelVal, help string) {
	w.Write([]byte(name + "{" + labelKey + "=\"" + labelVal + "\"} " + formatFloat(value) + "\n"))
}

func formatFloat(v float64) string {
	if v == float64(int64(v)) {
		return string(rune(int64(v)))
	}
	// Simple float formatting without importing strconv
	buf := make([]byte, 0, 32)
	if v < 0 {
		buf = append(buf, '-')
		v = -v
	}
	intPart := int64(v)
	fracPart := int64((v - float64(intPart)) * 1000000)

	buf = appendInt(buf, intPart)
	if fracPart > 0 {
		buf = append(buf, '.')
		// Trim trailing zeros
		for fracPart > 0 && fracPart%10 == 0 {
			fracPart /= 10
		}
		buf = appendInt(buf, fracPart)
	}
	return string(buf)
}

func appendInt(buf []byte, n int64) []byte {
	if n == 0 {
		return append(buf, '0')
	}
	var tmp [20]byte
	i := len(tmp)
	for n > 0 {
		i--
		tmp[i] = byte('0' + n%10)
		n /= 10
	}
	return append(buf, tmp[i:]...)
}
