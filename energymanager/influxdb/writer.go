// Package influxdb provides a lightweight InfluxDB line protocol writer
package influxdb

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/diebietse/invertergui/energymanager/core"
)

// Config for InfluxDB writer
type Config struct {
	// Server URL (e.g., "http://cloudyday.hedonistic.io:8086")
	URL string `json:"url"`
	// Database name
	Database string `json:"database"`
	// Optional authentication
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	// Optional authentication token (InfluxDB 2.x)
	Token string `json:"token,omitempty"`
	// Organization (InfluxDB 2.x)
	Org string `json:"org,omitempty"`
	// Bucket (InfluxDB 2.x, defaults to Database)
	Bucket string `json:"bucket,omitempty"`
	// Batch interval (how often to flush)
	BatchInterval time.Duration `json:"batch_interval"`
	// Maximum points per batch
	MaxBatchSize int `json:"max_batch_size"`
	// Measurement name
	Measurement string `json:"measurement"`
	// Tags added to all points
	Tags map[string]string `json:"tags,omitempty"`
	// Retry settings
	MaxRetries int           `json:"max_retries"`
	RetryDelay time.Duration `json:"retry_delay"`
}

// DefaultConfig returns sensible defaults
func DefaultConfig() Config {
	return Config{
		URL:           "http://localhost:8086",
		Database:      "energy",
		BatchInterval: 5 * time.Minute,
		MaxBatchSize:  1000,
		Measurement:   "energy_status",
		MaxRetries:    3,
		RetryDelay:    5 * time.Second,
	}
}

// Writer batches and sends data to InfluxDB
type Writer struct {
	config Config
	hub    *core.Hub
	sub    *core.Subscription
	client *http.Client

	mu     sync.Mutex
	buffer bytes.Buffer
	points int

	stopCh chan struct{}
	wg     sync.WaitGroup
}

// New creates a new InfluxDB writer
func New(hub *core.Hub, cfg Config) *Writer {
	if cfg.BatchInterval == 0 {
		cfg.BatchInterval = 5 * time.Minute
	}
	if cfg.MaxBatchSize == 0 {
		cfg.MaxBatchSize = 1000
	}
	if cfg.Measurement == "" {
		cfg.Measurement = "energy_status"
	}
	if cfg.MaxRetries == 0 {
		cfg.MaxRetries = 3
	}
	if cfg.RetryDelay == 0 {
		cfg.RetryDelay = 5 * time.Second
	}

	return &Writer{
		config: cfg,
		hub:    hub,
		client: &http.Client{Timeout: 30 * time.Second},
		stopCh: make(chan struct{}),
	}
}

// Start begins collecting and sending data
func (w *Writer) Start() error {
	// Subscribe to energy data
	w.sub = w.hub.Subscribe()

	w.wg.Add(2)

	// Collector goroutine
	go func() {
		defer w.wg.Done()
		for {
			select {
			case <-w.stopCh:
				return
			case info := <-w.sub.C():
				if info != nil {
					w.addPoint(info)
				}
			}
		}
	}()

	// Flusher goroutine
	go func() {
		defer w.wg.Done()
		ticker := time.NewTicker(w.config.BatchInterval)
		defer ticker.Stop()
		for {
			select {
			case <-w.stopCh:
				// Final flush
				w.flush()
				return
			case <-ticker.C:
				w.flush()
			}
		}
	}()

	log.Printf("influxdb: started writer to %s/%s (batch=%v)",
		w.config.URL, w.config.Database, w.config.BatchInterval)
	return nil
}

// Stop stops the writer
func (w *Writer) Stop() {
	close(w.stopCh)
	w.wg.Wait()
	if w.sub != nil {
		w.sub.Unsubscribe()
	}
}

// addPoint adds a point to the batch buffer
func (w *Writer) addPoint(info *core.EnergyInfo) {
	w.mu.Lock()
	defer w.mu.Unlock()

	// Build line protocol point
	// Format: measurement,tags fields timestamp
	// Example: energy_status,host=router grid_power=1234.5,bat_soc=85.0 1609459200000000000

	// Start with measurement and tags
	w.buffer.WriteString(w.config.Measurement)
	for k, v := range w.config.Tags {
		w.buffer.WriteString(",")
		w.buffer.WriteString(escapeTag(k))
		w.buffer.WriteString("=")
		w.buffer.WriteString(escapeTag(v))
	}

	// Fields
	w.buffer.WriteString(" ")
	first := true

	// Grid data
	if info.Grid.Valid {
		writeField(&w.buffer, &first, "grid_power", info.Grid.TotalPower)
		writeField(&w.buffer, &first, "grid_current", info.Grid.TotalCurrent)
		writeField(&w.buffer, &first, "grid_apparent", info.Grid.TotalApparent)
		for i, p := range info.Grid.Phases {
			prefix := fmt.Sprintf("phase%c_", 'a'+i)
			writeField(&w.buffer, &first, prefix+"voltage", p.Voltage)
			writeField(&w.buffer, &first, prefix+"current", p.Current)
			writeField(&w.buffer, &first, prefix+"power", p.ActivePower)
			writeField(&w.buffer, &first, prefix+"pf", p.PowerFactor)
		}
	}

	// Inverter data
	if info.Inverter.Valid {
		writeField(&w.buffer, &first, "bat_voltage", info.Inverter.BatVoltage)
		writeField(&w.buffer, &first, "bat_current", info.Inverter.BatCurrent)
		writeField(&w.buffer, &first, "bat_power", info.Inverter.BatPower)
		writeField(&w.buffer, &first, "bat_soc", info.Inverter.BatSOC)
		writeField(&w.buffer, &first, "ac_in_voltage", info.Inverter.ACInVoltage)
		writeField(&w.buffer, &first, "ac_out_voltage", info.Inverter.ACOutVoltage)
	}

	// Battery monitor data (BMV)
	if info.Battery.Valid {
		writeField(&w.buffer, &first, "bmv_voltage", info.Battery.VoltageV)
		writeField(&w.buffer, &first, "bmv_current", info.Battery.CurrentA)
		writeField(&w.buffer, &first, "bmv_power", info.Battery.PowerW)
		writeField(&w.buffer, &first, "bmv_soc", info.Battery.SOCPercent)
		writeField(&w.buffer, &first, "bmv_consumed_ah", info.Battery.ConsumedAh)
		writeField(&w.buffer, &first, "bmv_charged_kwh", info.Battery.ChargedKWh)
		writeField(&w.buffer, &first, "bmv_discharged_kwh", info.Battery.DischargedKWh)
	}

	// PID data
	if info.PID.Enabled {
		writeField(&w.buffer, &first, "pid_setpoint", info.PID.Setpoint)
		writeField(&w.buffer, &first, "pid_error", info.PID.Error)
		writeField(&w.buffer, &first, "pid_output", info.PID.Output)
	}

	// Mode data
	writeField(&w.buffer, &first, "mode_setpoint", info.Mode.AutoSetpoint)

	// Timestamp (nanoseconds)
	w.buffer.WriteString(" ")
	w.buffer.WriteString(fmt.Sprintf("%d", info.Timestamp.UnixNano()))
	w.buffer.WriteString("\n")

	w.points++

	// Flush if batch is full
	if w.points >= w.config.MaxBatchSize {
		go w.flushLocked()
	}
}

func writeField(buf *bytes.Buffer, first *bool, name string, value float64) {
	if *first {
		*first = false
	} else {
		buf.WriteString(",")
	}
	buf.WriteString(name)
	buf.WriteString("=")
	buf.WriteString(fmt.Sprintf("%g", value))
}

func escapeTag(s string) string {
	// Escape commas, spaces, and equals signs in tag keys/values
	result := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case ',', ' ', '=':
			result = append(result, '\\')
		}
		result = append(result, s[i])
	}
	return string(result)
}

// flush sends buffered data to InfluxDB
func (w *Writer) flush() {
	w.mu.Lock()
	w.flushLocked()
	w.mu.Unlock()
}

func (w *Writer) flushLocked() {
	if w.points == 0 {
		return
	}

	data := w.buffer.Bytes()
	points := w.points
	w.buffer.Reset()
	w.points = 0

	// Send in background
	go w.send(data, points)
}

func (w *Writer) send(data []byte, points int) {
	// Determine write URL
	var writeURL string
	if w.config.Token != "" || w.config.Org != "" {
		// InfluxDB 2.x
		bucket := w.config.Bucket
		if bucket == "" {
			bucket = w.config.Database
		}
		writeURL = fmt.Sprintf("%s/api/v2/write?org=%s&bucket=%s&precision=ns",
			w.config.URL, w.config.Org, bucket)
	} else {
		// InfluxDB 1.x
		writeURL = fmt.Sprintf("%s/write?db=%s&precision=ns",
			w.config.URL, w.config.Database)
	}

	var lastErr error
	for attempt := 0; attempt <= w.config.MaxRetries; attempt++ {
		if attempt > 0 {
			time.Sleep(w.config.RetryDelay * time.Duration(attempt))
		}

		req, err := http.NewRequest("POST", writeURL, bytes.NewReader(data))
		if err != nil {
			lastErr = err
			continue
		}

		req.Header.Set("Content-Type", "text/plain")

		// Authentication
		if w.config.Token != "" {
			req.Header.Set("Authorization", "Token "+w.config.Token)
		} else if w.config.Username != "" {
			req.SetBasicAuth(w.config.Username, w.config.Password)
		}

		resp, err := w.client.Do(req)
		if err != nil {
			lastErr = err
			log.Printf("influxdb: write failed (attempt %d): %v", attempt+1, err)
			continue
		}

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			resp.Body.Close()
			log.Printf("influxdb: wrote %d points", points)
			return
		}

		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		lastErr = fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
		log.Printf("influxdb: write failed (attempt %d): %v", attempt+1, lastErr)
	}

	log.Printf("influxdb: failed to write %d points after %d attempts: %v",
		points, w.config.MaxRetries+1, lastErr)
}

// Flush forces an immediate flush of buffered data
func (w *Writer) Flush() {
	w.flush()
}
