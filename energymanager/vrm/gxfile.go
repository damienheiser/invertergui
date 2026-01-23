package vrm

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// GXData represents the structure for a GX data file
type GXData struct {
	PortalID   string                 `json:"portal_id"`
	Identifier string                 `json:"identifier"`
	Name       string                 `json:"name"`
	Firmware   string                 `json:"firmware"`
	Hardware   string                 `json:"hardware"`
	Timestamp  int64                  `json:"timestamp"`
	Services   map[string]interface{} `json:"services"`
}

// GenerateGXFile creates a GX data file for VRM registration
// This file can be uploaded to VRM to register a new "GX device"
func GenerateGXFile(portalID, deviceName, outputPath string) error {
	now := time.Now()

	gxData := GXData{
		PortalID:   portalID,
		Identifier: portalID,
		Name:       deviceName,
		Firmware:   "v1.0.0",
		Hardware:   "OpenWRT/EnergyMonitor",
		Timestamp:  now.Unix(),
		Services: map[string]interface{}{
			"com.victronenergy.system": map[string]interface{}{
				"/DeviceInstance":  0,
				"/ProductId":       0,
				"/ProductName":     deviceName,
				"/FirmwareVersion": "1.0.0",
				"/Serial":          portalID,
				"/Connected":       1,
			},
			"com.victronenergy.grid": map[string]interface{}{
				"/DeviceInstance": 30,
				"/ProductId":      45069, // Carlo Gavazzi EM24 (common grid meter)
				"/ProductName":    "Grid Meter",
				"/Ac/Power":       0,
				"/Connected":      1,
			},
			"com.victronenergy.vebus": map[string]interface{}{
				"/DeviceInstance":  0,
				"/ProductId":       9763, // MultiPlus-II 48/5000
				"/ProductName":     "MultiPlus-II 48/5000",
				"/State":           0,
				"/Ac/ActiveIn/L1/P": 0,
				"/Dc/0/Voltage":    48.0,
				"/Soc":             50,
				"/Connected":       1,
			},
			"com.victronenergy.battery": map[string]interface{}{
				"/DeviceInstance": 0,
				"/ProductId":      0xA389, // SmartShunt
				"/ProductName":    "SmartShunt 500A/50mV",
				"/Dc/0/Voltage":   48.0,
				"/Dc/0/Current":   0,
				"/Soc":            50,
				"/Connected":      1,
			},
		},
	}

	data, err := json.MarshalIndent(gxData, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal GX data: %w", err)
	}

	if err := os.WriteFile(outputPath, data, 0644); err != nil {
		return fmt.Errorf("failed to write GX file: %w", err)
	}

	return nil
}
