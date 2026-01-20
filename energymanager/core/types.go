package core

import "time"

// OperatingMode defines how the system manages energy
type OperatingMode string

const (
	ModeManual      OperatingMode = "manual"       // Manual setpoint control
	ModePID         OperatingMode = "pid"          // Zero-grid PID control
	ModeTOU         OperatingMode = "tou"          // Time-of-Use optimization
	ModeSolar       OperatingMode = "solar"        // Maximize solar self-consumption
	ModeBatterySave OperatingMode = "battery_save" // Preserve battery for morning
	ModeScheduled   OperatingMode = "scheduled"    // Automatic mode switching based on schedule
)

// EnergyInfo contains all data from energy sources
type EnergyInfo struct {
	Timestamp time.Time `json:"timestamp"`
	Valid     bool      `json:"valid"`

	Grid     GridData     `json:"grid"`
	Inverter InverterData `json:"inverter"`
	Battery  BatteryData  `json:"battery"`
	PID      PIDData      `json:"pid"`
	Mode     ModeData     `json:"mode"`
	Config   ConfigData   `json:"config"`
}

// GridData from Shelly Pro EM3 (3-phase)
type GridData struct {
	TotalPower    float64      `json:"total_power_w"`     // W, positive=import, negative=export
	TotalCurrent  float64      `json:"total_current_a"`   // A
	TotalApparent float64      `json:"total_apparent_va"` // VA
	Phases        [3]PhaseData `json:"phases"`
	Valid         bool         `json:"valid"`
	Error         string       `json:"error,omitempty"`
}

// PhaseData for one AC phase
type PhaseData struct {
	Voltage       float64 `json:"voltage_v"`
	Current       float64 `json:"current_a"`
	ActivePower   float64 `json:"active_power_w"`
	ApparentPower float64 `json:"apparent_power_va"`
	PowerFactor   float64 `json:"power_factor"`
	Frequency     float64 `json:"frequency_hz"`
}

// InverterData from Victron MultiPlus
type InverterData struct {
	State        string  `json:"state"` // on, sleep, low_bat, temperature, overload, offline
	BatVoltage   float64 `json:"bat_voltage_v"`
	BatCurrent   float64 `json:"bat_current_a"` // positive=charging
	BatPower     float64 `json:"bat_power_w"`
	BatSOC       float64 `json:"bat_soc_pct"` // 0-100
	BatCapacity  float64 `json:"bat_capacity_kwh"` // Configured capacity
	BatRemaining float64 `json:"bat_remaining_kwh"` // Estimated remaining
	ACInVoltage  float64 `json:"ac_in_voltage_v"`
	ACInCurrent  float64 `json:"ac_in_current_a"`
	ACInFreq     float64 `json:"ac_in_freq_hz"`
	ACOutVoltage float64 `json:"ac_out_voltage_v"`
	ACOutCurrent float64 `json:"ac_out_current_a"`
	ACOutFreq    float64 `json:"ac_out_freq_hz"`
	Valid        bool    `json:"valid"`
	Error        string  `json:"error,omitempty"`
}

// BatteryData from Victron BMV/SmartShunt via VE.Direct
type BatteryData struct {
	Timestamp time.Time `json:"timestamp"`
	Valid     bool      `json:"valid"`

	// Current readings
	VoltageV    float64 `json:"voltage_v"`      // Main battery voltage
	AuxVoltageV float64 `json:"aux_voltage_v"`  // Starter/aux battery voltage
	CurrentA    float64 `json:"current_a"`      // Current (negative = discharging)
	PowerW      float64 `json:"power_w"`        // Instantaneous power
	ConsumedAh  float64 `json:"consumed_ah"`    // Ah consumed since full
	SOCPercent  float64 `json:"soc_percent"`    // State of charge 0-100
	TimeToGoMin int     `json:"time_to_go_min"` // Minutes until empty (-1 = infinite)

	// Temperature (if available)
	TemperatureC   float64 `json:"temperature_c,omitempty"`
	HasTemperature bool    `json:"has_temperature"`

	// Alarm state
	AlarmActive bool   `json:"alarm_active"`
	AlarmReason string `json:"alarm_reason,omitempty"`

	// Lifetime statistics
	ChargedKWh    float64 `json:"charged_kwh"`    // Total charged energy
	DischargedKWh float64 `json:"discharged_kwh"` // Total discharged energy
	ChargeCycles  int     `json:"charge_cycles"`
	FullDischarges int    `json:"full_discharges"`
	TotalAhDrawn  float64 `json:"total_ah_drawn"`

	// Voltage history
	MinVoltageV float64 `json:"min_voltage_v"`
	MaxVoltageV float64 `json:"max_voltage_v"`

	// Discharge history
	DeepestDischargeAh float64 `json:"deepest_discharge_ah"`
	LastDischargeAh    float64 `json:"last_discharge_ah"`
	AvgDischargeAh     float64 `json:"avg_discharge_ah"`

	// Timing
	SecondsSinceFullCharge int `json:"seconds_since_full_charge"`
	AutoSyncs              int `json:"auto_syncs"`

	// Alarm counts
	LowVoltageAlarms  int `json:"low_voltage_alarms"`
	HighVoltageAlarms int `json:"high_voltage_alarms"`

	// Device info
	ProductID       string `json:"product_id"`
	ProductName     string `json:"product_name"`
	FirmwareVersion string `json:"firmware_version"`
	SerialNumber    string `json:"serial_number"`

	Error string `json:"error,omitempty"`
}

// PIDData shows PID controller state
type PIDData struct {
	Setpoint  float64 `json:"setpoint_w"`   // Target grid power (0W)
	GridPower float64 `json:"grid_power_w"` // Measured grid power
	Error     float64 `json:"error_w"`      // Setpoint - GridPower
	Output    float64 `json:"output_w"`     // Command sent to inverter
	Enabled   bool    `json:"enabled"`
}

// ModeData shows current operating mode status
type ModeData struct {
	Current      OperatingMode `json:"current"`
	Description  string        `json:"description"`
	AutoSetpoint float64       `json:"auto_setpoint_w"` // Calculated setpoint for current mode
	Override     bool          `json:"override"`         // Manual override active
	OverrideVal  float64       `json:"override_value_w"` // Manual override value

	// Schedule specific
	ScheduleEnabled bool   `json:"schedule_enabled,omitempty"`
	SchedulePeriod  string `json:"schedule_period,omitempty"` // Current schedule period name
	ScheduleAction  string `json:"schedule_action,omitempty"` // Current action from schedule

	// TOU specific
	TOUPeriod string  `json:"tou_period,omitempty"` // peak, offpeak, shoulder
	TOURate   float64 `json:"tou_rate,omitempty"`   // Current rate $/kWh

	// Solar specific
	SolarPower float64 `json:"solar_power_w,omitempty"`
	ZeroExport bool    `json:"zero_export,omitempty"` // Whether zero-export is active

	// Battery-save specific
	MinutesToSunrise int     `json:"minutes_to_sunrise,omitempty"`
	TargetDischarge  float64 `json:"target_discharge_w,omitempty"`
}

// ConfigData contains current configuration (for UI)
type ConfigData struct {
	ShellyAddr       string  `json:"shelly_addr"`
	ShellyConnected  bool    `json:"shelly_connected"`
	VictronPort      string  `json:"victron_port"`
	VictronConnected bool    `json:"victron_connected"`
	BMVPort          string  `json:"bmv_port"`
	BMVConnected     bool    `json:"bmv_connected"`
	MaxPower         float64 `json:"max_power_w"`
	BatteryCapacity  float64 `json:"battery_capacity_kwh"`
}

// DataSource interface for energy data providers
type DataSource interface {
	C() <-chan *EnergyInfo
	Close()
}

// InverterController interface for controlling the inverter
type InverterController interface {
	SetPower(watts float64) error
	Wakeup() error
	Sleep() error
}

// TOUSchedule defines time-of-use pricing periods
type TOUSchedule struct {
	Periods []TOUPeriod `json:"periods"`
}

// TOUPeriod defines a time period with pricing
type TOUPeriod struct {
	Name      string  `json:"name"`       // peak, offpeak, shoulder
	StartHour int     `json:"start_hour"` // 0-23
	EndHour   int     `json:"end_hour"`   // 0-23
	Days      []int   `json:"days"`       // 0=Sun, 1=Mon, etc.
	Rate      float64 `json:"rate"`       // $/kWh
	Action    string  `json:"action"`     // charge, discharge, hold
}

// ModeSchedule defines automated mode switching based on time
type ModeSchedule struct {
	Enabled bool             `json:"enabled"`
	Periods []SchedulePeriod `json:"periods"`
}

// SchedulePeriod defines a time period with mode and action
type SchedulePeriod struct {
	Name      string       `json:"name"`
	StartTime ScheduleTime `json:"start_time"`
	EndTime   ScheduleTime `json:"end_time"`
	Days      []int        `json:"days"`     // 0=Sun, 1=Mon, etc. Empty = all days
	Mode      string       `json:"mode"`     // pid, solar, battery_save, tou
	Action    string       `json:"action"`   // charge, discharge, hold, solar_first (optional override)
	Rate      float64      `json:"rate"`     // $/kWh for display
	Priority  int          `json:"priority"` // Higher = takes precedence on overlap
}

// ScheduleTime represents a time that can be absolute or relative to sunrise/sunset
type ScheduleTime struct {
	Type   string `json:"type"`   // "absolute", "sunrise", "sunset"
	Hour   int    `json:"hour"`   // For absolute: 0-23
	Minute int    `json:"minute"` // For absolute: 0-59
	Offset int    `json:"offset"` // Minutes offset for sunrise/sunset (can be negative)
}

// SolarConfig for solar mode
type SolarConfig struct {
	Enabled       bool    `json:"enabled"`
	ZeroExport    bool    `json:"zero_export"`      // If true, never export to grid
	ExportLimit   float64 `json:"export_limit_w"`   // Max export to grid (-1 = unlimited, only used if ZeroExport=false)
	MinSOC        float64 `json:"min_soc_pct"`      // Don't discharge below
	MaxSOC        float64 `json:"max_soc_pct"`      // Don't charge above
	PreferSelfUse bool    `json:"prefer_self_use"`  // Prioritize house loads (solar powers loads before battery)
}

// BatterySaveConfig for battery save mode
type BatterySaveConfig struct {
	Enabled         bool    `json:"enabled"`
	MinSOC          float64 `json:"min_soc_pct"`          // Reserve SOC
	SunriseOffset   int     `json:"sunrise_offset_min"`   // Minutes after sunrise to end
	TargetSOC       float64 `json:"target_soc_at_sunrise"` // Target SOC at sunrise
}
