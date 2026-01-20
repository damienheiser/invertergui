// Package rrd provides an in-memory round-robin database for metrics
package rrd

import (
	"sync"
	"time"
)

// Sample represents a single data point
type Sample struct {
	Timestamp time.Time `json:"timestamp"`
	GridPower float64   `json:"grid_power_w"`
	BatPower  float64   `json:"bat_power_w"`
	BatSOC    float64   `json:"bat_soc_pct"`
	BatVoltage float64  `json:"bat_voltage_v"`
	SolarPower float64  `json:"solar_power_w"` // Calculated from grid + bat
	ImportWh  float64   `json:"import_wh"`     // Accumulated
	ExportWh  float64   `json:"export_wh"`     // Accumulated
}

// Archive stores samples at a specific resolution
type Archive struct {
	Resolution time.Duration
	Samples    []Sample
	MaxSamples int
	Position   int // Next write position (circular)
	Count      int // Number of valid samples
}

// RRD is an in-memory round-robin database
type RRD struct {
	mu       sync.RWMutex
	archives []*Archive

	// Accumulation state for energy totals
	lastSample   time.Time
	lastGridPower float64
	totalImportWh float64
	totalExportWh float64
}

// Config defines RRD archives
type Config struct {
	// Each archive defines resolution and retention
	// Default: 5-minute samples for 24h, 1-hour samples for 30 days
	Archives []ArchiveConfig
}

// ArchiveConfig defines a single archive
type ArchiveConfig struct {
	Resolution time.Duration
	Retention  time.Duration
}

// DefaultConfig returns sensible defaults for router use
func DefaultConfig() Config {
	return Config{
		Archives: []ArchiveConfig{
			{Resolution: 5 * time.Minute, Retention: 24 * time.Hour},    // 288 samples
			{Resolution: time.Hour, Retention: 30 * 24 * time.Hour},     // 720 samples
		},
	}
}

// New creates a new RRD with the given configuration
func New(cfg Config) *RRD {
	if len(cfg.Archives) == 0 {
		cfg = DefaultConfig()
	}

	archives := make([]*Archive, len(cfg.Archives))
	for i, ac := range cfg.Archives {
		maxSamples := int(ac.Retention / ac.Resolution)
		if maxSamples < 1 {
			maxSamples = 1
		}
		archives[i] = &Archive{
			Resolution: ac.Resolution,
			Samples:    make([]Sample, maxSamples),
			MaxSamples: maxSamples,
		}
	}

	return &RRD{
		archives: archives,
	}
}

// Add adds a new sample, aggregating into appropriate archives
func (r *RRD) Add(s Sample) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Calculate energy accumulation
	now := s.Timestamp
	if !r.lastSample.IsZero() {
		duration := now.Sub(r.lastSample).Hours()
		if duration > 0 && duration < 1 { // Sanity check (less than 1 hour gap)
			avgPower := (r.lastGridPower + s.GridPower) / 2
			if avgPower > 0 {
				r.totalImportWh += avgPower * duration
			} else {
				r.totalExportWh += -avgPower * duration
			}
		}
	}
	r.lastSample = now
	r.lastGridPower = s.GridPower

	// Update accumulated values in sample
	s.ImportWh = r.totalImportWh
	s.ExportWh = r.totalExportWh

	// Add to all archives that need updating
	for _, arch := range r.archives {
		r.addToArchive(arch, s)
	}
}

func (r *RRD) addToArchive(arch *Archive, s Sample) {
	// Check if we should add to this archive
	// (enough time has passed since last sample)
	if arch.Count > 0 {
		lastIdx := (arch.Position - 1 + arch.MaxSamples) % arch.MaxSamples
		lastSample := arch.Samples[lastIdx]
		if s.Timestamp.Sub(lastSample.Timestamp) < arch.Resolution {
			return // Not enough time passed
		}
	}

	// Add sample at current position
	arch.Samples[arch.Position] = s
	arch.Position = (arch.Position + 1) % arch.MaxSamples
	if arch.Count < arch.MaxSamples {
		arch.Count++
	}
}

// GetSamples returns samples from the specified archive (0 = highest resolution)
func (r *RRD) GetSamples(archiveIndex int) []Sample {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if archiveIndex < 0 || archiveIndex >= len(r.archives) {
		return nil
	}

	arch := r.archives[archiveIndex]
	if arch.Count == 0 {
		return nil
	}

	// Return samples in chronological order
	result := make([]Sample, arch.Count)
	startIdx := (arch.Position - arch.Count + arch.MaxSamples) % arch.MaxSamples
	for i := 0; i < arch.Count; i++ {
		idx := (startIdx + i) % arch.MaxSamples
		result[i] = arch.Samples[idx]
	}
	return result
}

// GetLatest returns the most recent sample from highest resolution archive
func (r *RRD) GetLatest() *Sample {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if len(r.archives) == 0 || r.archives[0].Count == 0 {
		return nil
	}

	arch := r.archives[0]
	lastIdx := (arch.Position - 1 + arch.MaxSamples) % arch.MaxSamples
	s := arch.Samples[lastIdx]
	return &s
}

// GetStats returns current statistics
func (r *RRD) GetStats() Stats {
	r.mu.RLock()
	defer r.mu.RUnlock()

	archiveStats := make([]ArchiveStats, len(r.archives))
	for i, arch := range r.archives {
		archiveStats[i] = ArchiveStats{
			Resolution: arch.Resolution,
			Count:      arch.Count,
			MaxSamples: arch.MaxSamples,
		}
	}

	return Stats{
		TotalImportWh: r.totalImportWh,
		TotalExportWh: r.totalExportWh,
		Archives:      archiveStats,
	}
}

// Stats contains RRD statistics
type Stats struct {
	TotalImportWh float64        `json:"total_import_wh"`
	TotalExportWh float64        `json:"total_export_wh"`
	Archives      []ArchiveStats `json:"archives"`
}

// ArchiveStats contains stats for a single archive
type ArchiveStats struct {
	Resolution time.Duration `json:"resolution"`
	Count      int           `json:"count"`
	MaxSamples int           `json:"max_samples"`
}

// Reset clears all data
func (r *RRD) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, arch := range r.archives {
		arch.Position = 0
		arch.Count = 0
	}
	r.totalImportWh = 0
	r.totalExportWh = 0
	r.lastSample = time.Time{}
}
