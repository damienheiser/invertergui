package pid

import (
	"log"
	"math"
	"sync"
	"time"
)

// Controller implements a PID controller for energy management
// Goal: Keep grid power at setpoint (typically 0W)
// Input: Grid power from energy meter (positive = import, negative = export)
// Output: Inverter command (positive = charge from grid, negative = feed to grid)
type Controller struct {
	mu sync.RWMutex

	// PID gains
	Kp float64 // Proportional gain
	Ki float64 // Integral gain
	Kd float64 // Derivative gain

	// Setpoint (target grid power in watts, typically 0)
	Setpoint float64

	// Output limits
	MinOutput float64 // Minimum output (max feed to grid, e.g., -5000)
	MaxOutput float64 // Maximum output (max charge, e.g., +5000)

	// Anti-windup: integral limits
	MaxIntegral float64

	// Internal state
	lastError  float64
	integral   float64
	lastOutput float64
	lastTime   time.Time

	// Deadband: ignore small errors
	Deadband float64

	// Rate limiting
	MaxRateOfChange float64 // Max W/s change

	// Enable/disable
	enabled bool
}

// Config for creating a new PID controller
type Config struct {
	Kp              float64
	Ki              float64
	Kd              float64
	Setpoint        float64
	MinOutput       float64
	MaxOutput       float64
	MaxIntegral     float64
	Deadband        float64
	MaxRateOfChange float64
}

// DefaultConfig returns sensible defaults for energy management
func DefaultConfig() Config {
	return Config{
		Kp:              1.1,   // Slightly over 1:1 to compensate for losses
		Ki:              0.15,  // Integral to eliminate steady-state error
		Kd:              0.05,  // Small derivative for smoothing
		Setpoint:        0,     // Target 0W grid power
		MinOutput:       -5000, // Max 5000W feed to grid
		MaxOutput:       5000,  // Max 5000W charge from grid
		MaxIntegral:     5000,  // Allow full integral range
		Deadband:        10,    // Ignore errors < 10W
		MaxRateOfChange: 2000,  // Allow 2000W/s change for faster response
	}
}

// New creates a new PID controller
func New(cfg Config) *Controller {
	return &Controller{
		Kp:              cfg.Kp,
		Ki:              cfg.Ki,
		Kd:              cfg.Kd,
		Setpoint:        cfg.Setpoint,
		MinOutput:       cfg.MinOutput,
		MaxOutput:       cfg.MaxOutput,
		MaxIntegral:     cfg.MaxIntegral,
		Deadband:        cfg.Deadband,
		MaxRateOfChange: cfg.MaxRateOfChange,
		enabled:         true,
	}
}

// Update computes new output based on current grid power
// Returns the inverter command in watts
func (c *Controller) Update(gridPower float64) float64 {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.enabled {
		return 0
	}

	now := time.Now()
	dt := now.Sub(c.lastTime).Seconds()
	if dt <= 0 || dt > 10 {
		dt = 1 // Default to 1 second if time is invalid
	}
	c.lastTime = now

	// Error: positive error means we're importing (need to discharge)
	// negative error means we're exporting (need to charge)
	error := c.Setpoint - gridPower

	// Apply deadband
	if math.Abs(error) < c.Deadband {
		error = 0
	}

	// Proportional term
	pTerm := c.Kp * error

	// Integral term with anti-windup
	c.integral += error * dt
	c.integral = clamp(c.integral, -c.MaxIntegral, c.MaxIntegral)
	iTerm := c.Ki * c.integral

	// Derivative term (on error, not on setpoint change)
	dTerm := 0.0
	if dt > 0 {
		dTerm = c.Kd * (error - c.lastError) / dt
	}
	c.lastError = error

	// Calculate raw output
	output := pTerm + iTerm + dTerm

	// Rate limiting
	if c.MaxRateOfChange > 0 {
		maxChange := c.MaxRateOfChange * dt
		if output-c.lastOutput > maxChange {
			output = c.lastOutput + maxChange
		} else if c.lastOutput-output > maxChange {
			output = c.lastOutput - maxChange
		}
	}

	// Clamp to output limits
	output = clamp(output, c.MinOutput, c.MaxOutput)

	// The output is inverted:
	// If grid is positive (importing), error is negative, we need to discharge (negative command)
	// If grid is negative (exporting), error is positive, we need to charge (positive command)
	// So: output = -error_response
	// Since error = setpoint - gridPower = 0 - gridPower = -gridPower
	// And we want: gridPower + inverterPower ≈ 0
	// So: inverterPower ≈ -gridPower
	// The PID already gives us: output ≈ -gridPower (when Kp=1, Ki=0, Kd=0)

	c.lastOutput = output

	log.Printf("pid: grid=%.0fW error=%.0fW output=%.0fW (P=%.0f I=%.0f D=%.0f)",
		gridPower, error, output, pTerm, iTerm, dTerm)

	return output
}

// SetEnabled enables or disables the controller
func (c *Controller) SetEnabled(enabled bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.enabled = enabled
	if !enabled {
		c.integral = 0
		c.lastOutput = 0
	}
}

// IsEnabled returns whether the controller is enabled
func (c *Controller) IsEnabled() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.enabled
}

// SetSetpoint changes the target grid power
func (c *Controller) SetSetpoint(watts float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Setpoint = watts
}

// SetGains updates PID gains
func (c *Controller) SetGains(kp, ki, kd float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Kp = kp
	c.Ki = ki
	c.Kd = kd
}

// Reset clears the controller state
func (c *Controller) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.integral = 0
	c.lastError = 0
	c.lastOutput = 0
	c.lastTime = time.Time{}
}

// State returns current controller state for monitoring
func (c *Controller) State() (setpoint, lastError, integral, output float64, enabled bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.Setpoint, c.lastError, c.integral, c.lastOutput, c.enabled
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
