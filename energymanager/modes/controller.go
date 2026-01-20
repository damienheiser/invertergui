package modes

import (
	"log"
	"math"
	"sync"
	"time"

	"github.com/diebietse/invertergui/energymanager/core"
)

// Controller manages operating modes and calculates setpoints
type Controller struct {
	mu sync.RWMutex

	// Current mode
	mode core.OperatingMode

	// Override
	overrideEnabled bool
	overrideValue   float64

	// Configuration
	maxPower        float64
	batteryCapacity float64 // kWh
	latitude        float64
	longitude       float64

	// TOU configuration
	touSchedule core.TOUSchedule

	// Solar configuration
	solarConfig core.SolarConfig

	// Battery-save configuration
	batterySaveConfig core.BatterySaveConfig

	// Mode schedule configuration
	modeSchedule core.ModeSchedule

	// Current state
	currentSOC float64

	// Cached sunrise/sunset times (recalculated daily)
	sunriseCache time.Time
	sunsetCache  time.Time
	lastSunCalc  time.Time
}

// Config for the mode controller
type Config struct {
	MaxPower        float64
	BatteryCapacity float64
	Latitude        float64
	Longitude       float64
}

// New creates a new mode controller
func New(cfg Config) *Controller {
	return &Controller{
		mode:            core.ModePID,
		maxPower:        cfg.MaxPower,
		batteryCapacity: cfg.BatteryCapacity,
		latitude:        cfg.Latitude,
		longitude:       cfg.Longitude,
		solarConfig: core.SolarConfig{
			ZeroExport:    true, // Default to zero export
			ExportLimit:   0,
			MinSOC:        20,
			MaxSOC:        100,
			PreferSelfUse: true,
		},
		batterySaveConfig: core.BatterySaveConfig{
			MinSOC:        20,
			SunriseOffset: 30,
			TargetSOC:     30,
		},
		modeSchedule: core.ModeSchedule{
			Enabled: false,
		},
	}
}

// SetMode changes the operating mode
func (c *Controller) SetMode(mode core.OperatingMode) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.mode = mode
	log.Printf("modes: changed to %s", mode)
}

// GetMode returns current mode
func (c *Controller) GetMode() core.OperatingMode {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.mode
}

// SetOverride enables manual setpoint override
func (c *Controller) SetOverride(enabled bool, value float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.overrideEnabled = enabled
	c.overrideValue = value
	log.Printf("modes: override enabled=%v value=%.0fW", enabled, value)
}

// SetTOUSchedule updates TOU schedule
func (c *Controller) SetTOUSchedule(schedule core.TOUSchedule) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.touSchedule = schedule
}

// SetSolarConfig updates solar configuration
func (c *Controller) SetSolarConfig(cfg core.SolarConfig) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.solarConfig = cfg
}

// SetBatterySaveConfig updates battery-save configuration
func (c *Controller) SetBatterySaveConfig(cfg core.BatterySaveConfig) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.batterySaveConfig = cfg
}

// SetModeSchedule updates the mode schedule configuration
func (c *Controller) SetModeSchedule(schedule core.ModeSchedule) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.modeSchedule = schedule
	if schedule.Enabled {
		log.Printf("modes: schedule enabled with %d periods", len(schedule.Periods))
	}
}

// GetModeSchedule returns the current mode schedule
func (c *Controller) GetModeSchedule() core.ModeSchedule {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.modeSchedule
}

// UpdateSOC updates the current battery state of charge
func (c *Controller) UpdateSOC(soc float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.currentSOC = soc
}

// Calculate computes the setpoint for the current mode
// Returns (setpoint, modeData)
func (c *Controller) Calculate(gridPower float64) (float64, core.ModeData) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	data := core.ModeData{
		Current:     c.mode,
		Override:    c.overrideEnabled,
		OverrideVal: c.overrideValue,
	}

	// If override is enabled, use that value
	if c.overrideEnabled {
		data.AutoSetpoint = c.overrideValue
		data.Description = "Manual override active"
		return c.overrideValue, data
	}

	var setpoint float64
	effectiveMode := c.mode

	// If in scheduled mode, determine the effective mode from schedule
	if c.mode == core.ModeScheduled && c.modeSchedule.Enabled {
		period := c.findCurrentSchedulePeriod()
		if period != nil {
			data.ScheduleEnabled = true
			data.SchedulePeriod = period.Name
			data.ScheduleAction = period.Action
			effectiveMode = core.OperatingMode(period.Mode)
			data.Current = effectiveMode
		} else {
			// No matching period, fall back to PID
			effectiveMode = core.ModePID
			data.ScheduleEnabled = true
			data.SchedulePeriod = "none"
		}
	}

	switch effectiveMode {
	case core.ModeManual:
		setpoint = 0
		data.Description = "Manual mode - no automatic control"

	case core.ModePID:
		setpoint = 0
		data.Description = "Zero-grid PID control"

	case core.ModeTOU:
		setpoint, data = c.calculateTOU(data)

	case core.ModeSolar:
		setpoint, data = c.calculateSolar(gridPower, data)

	case core.ModeBatterySave:
		setpoint, data = c.calculateBatterySave(data)

	default:
		setpoint = 0
		data.Description = "Unknown mode"
	}

	// Clamp to max power
	setpoint = clamp(setpoint, -c.maxPower, c.maxPower)
	data.AutoSetpoint = setpoint

	return setpoint, data
}

func (c *Controller) calculateTOU(data core.ModeData) (float64, core.ModeData) {
	now := time.Now()
	hour := now.Hour()
	weekday := int(now.Weekday())

	// Find current period
	var currentPeriod *core.TOUPeriod
	for i := range c.touSchedule.Periods {
		p := &c.touSchedule.Periods[i]
		if hour >= p.StartHour && hour < p.EndHour {
			for _, d := range p.Days {
				if d == weekday {
					currentPeriod = p
					break
				}
			}
		}
		if currentPeriod != nil {
			break
		}
	}

	if currentPeriod == nil {
		data.Description = "TOU: No matching period"
		data.TOUPeriod = "none"
		return 0, data
	}

	data.TOUPeriod = currentPeriod.Name
	data.TOURate = currentPeriod.Rate

	var setpoint float64
	switch currentPeriod.Action {
	case "charge":
		// Charge from grid at max rate
		setpoint = c.maxPower
		data.Description = "TOU: Charging (off-peak)"
	case "discharge":
		// Discharge to grid at max rate
		setpoint = -c.maxPower
		data.Description = "TOU: Discharging (peak)"
	default:
		// Hold - zero grid
		setpoint = 0
		data.Description = "TOU: Holding (shoulder)"
	}

	// Respect SOC limits
	if c.currentSOC >= 100 && setpoint > 0 {
		setpoint = 0
		data.Description = "TOU: Battery full"
	}
	if c.currentSOC <= c.batterySaveConfig.MinSOC && setpoint < 0 {
		setpoint = 0
		data.Description = "TOU: Battery low"
	}

	return setpoint, data
}

func (c *Controller) calculateSolar(gridPower float64, data core.ModeData) (float64, core.ModeData) {
	data.ZeroExport = c.solarConfig.ZeroExport

	// Solar Mode Priority:
	// 1. Use solar to power house loads first (solar-first)
	// 2. Charge battery with excess solar
	// 3. If ZeroExport enabled: NEVER export to grid, charge battery instead
	// 4. If ZeroExport disabled: respect ExportLimit
	// 5. Don't discharge battery below MinSOC

	// Check if battery is too low to discharge
	if c.currentSOC <= c.solarConfig.MinSOC {
		if gridPower < 0 {
			// Exporting (solar excess) - charge battery with all excess
			data.Description = "Solar: Low SOC, charging with solar excess"
			return math.Min(-gridPower, c.maxPower), data
		}
		// Importing - battery too low to help
		data.Description = "Solar: Low SOC, battery protection"
		return 0, data
	}

	// Check if battery is full
	if c.currentSOC >= c.solarConfig.MaxSOC {
		if c.solarConfig.ZeroExport && gridPower < -20 {
			// Battery full but we need to absorb excess to prevent export
			// This is a limitation - can't absorb if battery is full
			data.Description = "Solar: Battery full, limiting production"
			// Return small charge to indicate we want zero grid (PID will limit)
			return 0, data
		}
		data.Description = "Solar: Battery full"
		return 0, data
	}

	// Normal operation
	var setpoint float64

	if gridPower < 0 {
		// We're exporting (solar excess)
		if c.solarConfig.ZeroExport {
			// Zero export mode: charge battery with ALL solar excess
			// setpoint = how much to charge = negative of grid power (which is negative when exporting)
			setpoint = -gridPower // Positive = charge battery
			data.Description = "Solar: Zero-export, charging battery"
		} else if c.solarConfig.ExportLimit > 0 && gridPower < -c.solarConfig.ExportLimit {
			// Limited export mode: only charge if we exceed limit
			setpoint = -gridPower - c.solarConfig.ExportLimit
			data.Description = "Solar: Export limit active"
		} else {
			// Allow unlimited export
			data.Description = "Solar: Exporting excess"
			setpoint = 0
		}
	} else if gridPower > 50 {
		// We're importing significantly - discharge battery to cover house load
		// In solar mode, we prioritize solar, but can use battery for loads
		if c.solarConfig.PreferSelfUse {
			// Discharge to cover the import
			setpoint = -gridPower // Negative = discharge
			data.Description = "Solar: Self-consumption from battery"
		} else {
			data.Description = "Solar: Allowing grid import"
			setpoint = 0
		}
	} else {
		// Balanced (within deadband)
		data.Description = "Solar: Balanced"
		setpoint = 0
	}

	return clamp(setpoint, -c.maxPower, c.maxPower), data
}

func (c *Controller) calculateBatterySave(data core.ModeData) (float64, core.ModeData) {
	// Calculate minutes until sunrise
	sunrise := c.calculateSunrise()
	now := time.Now()

	var minutesToSunrise int
	if sunrise.After(now) {
		minutesToSunrise = int(sunrise.Sub(now).Minutes())
	} else {
		// After sunrise, behave like PID mode
		data.Description = "Battery-save: After sunrise, zero-grid mode"
		data.MinutesToSunrise = 0
		return 0, data
	}

	data.MinutesToSunrise = minutesToSunrise

	// Calculate how much energy we need to discharge
	// (currentSOC - targetSOC) * capacity = kWh to discharge
	socDiff := c.currentSOC - c.batterySaveConfig.TargetSOC
	if socDiff <= 0 {
		data.Description = "Battery-save: At target SOC, holding"
		return 0, data
	}

	kwhToDischarge := (socDiff / 100) * c.batteryCapacity

	// Calculate average discharge rate needed
	// kWh / hours = kW
	hoursToSunrise := float64(minutesToSunrise) / 60.0
	if hoursToSunrise <= 0 {
		hoursToSunrise = 0.1 // Minimum 6 minutes
	}

	targetDischargeKW := kwhToDischarge / hoursToSunrise
	targetDischargeW := targetDischargeKW * 1000

	// Negative setpoint = discharge
	setpoint := -clamp(targetDischargeW, 0, c.maxPower)

	data.TargetDischarge = -setpoint
	data.Description = "Battery-save: Gradual discharge until sunrise"

	log.Printf("modes: battery-save: SOC=%.0f%% target=%.0f%% kWh=%.2f hours=%.1f discharge=%.0fW",
		c.currentSOC, c.batterySaveConfig.TargetSOC, kwhToDischarge, hoursToSunrise, -setpoint)

	return setpoint, data
}

// calculateSunrise returns approximate sunrise time for today
func (c *Controller) calculateSunrise() time.Time {
	now := time.Now()
	year, month, day := now.Date()
	loc := now.Location()

	// Simplified sunrise calculation
	// For more accuracy, use a proper solar position library

	// Day of year
	dayOfYear := now.YearDay()

	// Solar declination (approximate)
	declination := 23.45 * math.Sin(2*math.Pi*(284+float64(dayOfYear))/365)
	declinationRad := declination * math.Pi / 180
	latRad := c.latitude * math.Pi / 180

	// Hour angle at sunrise
	cosHourAngle := -math.Tan(latRad) * math.Tan(declinationRad)
	cosHourAngle = clamp(cosHourAngle, -1, 1)
	hourAngle := math.Acos(cosHourAngle)

	// Sunrise hour (in hours from midnight, solar time)
	sunriseHour := 12 - (hourAngle * 180 / math.Pi / 15)

	// Approximate timezone correction (very rough)
	sunriseHour += c.longitude / 15

	// Add offset
	sunriseHour += float64(c.batterySaveConfig.SunriseOffset) / 60.0

	hours := int(sunriseHour)
	minutes := int((sunriseHour - float64(hours)) * 60)

	return time.Date(year, month, day, hours, minutes, 0, 0, loc)
}

// calculateSunset returns approximate sunset time for today
func (c *Controller) calculateSunset() time.Time {
	now := time.Now()
	year, month, day := now.Date()
	loc := now.Location()

	dayOfYear := now.YearDay()

	declination := 23.45 * math.Sin(2*math.Pi*(284+float64(dayOfYear))/365)
	declinationRad := declination * math.Pi / 180
	latRad := c.latitude * math.Pi / 180

	cosHourAngle := -math.Tan(latRad) * math.Tan(declinationRad)
	cosHourAngle = clamp(cosHourAngle, -1, 1)
	hourAngle := math.Acos(cosHourAngle)

	// Sunset hour (in hours from midnight, solar time)
	sunsetHour := 12 + (hourAngle * 180 / math.Pi / 15)

	// Approximate timezone correction
	sunsetHour += c.longitude / 15

	hours := int(sunsetHour)
	minutes := int((sunsetHour - float64(hours)) * 60)

	return time.Date(year, month, day, hours, minutes, 0, 0, loc)
}

// getSunriseSunset returns cached or freshly calculated sunrise/sunset times
func (c *Controller) getSunriseSunset() (sunrise, sunset time.Time) {
	now := time.Now()

	// Recalculate if cache is from a different day
	if c.lastSunCalc.IsZero() || c.lastSunCalc.Day() != now.Day() {
		c.sunriseCache = c.calculateSunrise()
		c.sunsetCache = c.calculateSunset()
		c.lastSunCalc = now
	}

	return c.sunriseCache, c.sunsetCache
}

// resolveScheduleTime converts a ScheduleTime to an absolute time for today
func (c *Controller) resolveScheduleTime(st core.ScheduleTime) time.Time {
	now := time.Now()
	year, month, day := now.Date()
	loc := now.Location()

	switch st.Type {
	case "sunrise":
		sunrise, _ := c.getSunriseSunset()
		return sunrise.Add(time.Duration(st.Offset) * time.Minute)
	case "sunset":
		_, sunset := c.getSunriseSunset()
		return sunset.Add(time.Duration(st.Offset) * time.Minute)
	default: // "absolute"
		return time.Date(year, month, day, st.Hour, st.Minute, 0, 0, loc)
	}
}

// findCurrentSchedulePeriod finds the active schedule period
func (c *Controller) findCurrentSchedulePeriod() *core.SchedulePeriod {
	if !c.modeSchedule.Enabled || len(c.modeSchedule.Periods) == 0 {
		return nil
	}

	now := time.Now()
	weekday := int(now.Weekday())

	var bestMatch *core.SchedulePeriod
	bestPriority := -1

	for i := range c.modeSchedule.Periods {
		p := &c.modeSchedule.Periods[i]

		// Check if day matches (empty days = all days)
		dayMatches := len(p.Days) == 0
		for _, d := range p.Days {
			if d == weekday {
				dayMatches = true
				break
			}
		}
		if !dayMatches {
			continue
		}

		// Resolve start and end times
		startTime := c.resolveScheduleTime(p.StartTime)
		endTime := c.resolveScheduleTime(p.EndTime)

		// Handle overnight periods (end time before start time)
		if endTime.Before(startTime) || endTime.Equal(startTime) {
			// Period spans midnight
			// Check if we're after start OR before end
			if now.After(startTime) || now.Before(endTime) {
				if p.Priority > bestPriority {
					bestMatch = p
					bestPriority = p.Priority
				}
			}
		} else {
			// Normal period within same day
			if now.After(startTime) && now.Before(endTime) {
				if p.Priority > bestPriority {
					bestMatch = p
					bestPriority = p.Priority
				}
			}
		}
	}

	return bestMatch
}

func clamp(v, min, max float64) float64 {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}
