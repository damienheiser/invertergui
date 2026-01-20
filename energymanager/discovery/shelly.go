package discovery

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ShellyDevice represents a discovered Shelly device
type ShellyDevice struct {
	IP       string `json:"ip"`
	MAC      string `json:"mac"`
	Type     string `json:"type"`
	Name     string `json:"name"`
	Model    string `json:"model"`
	Gen      int    `json:"gen"`      // 1 or 2
	HasEM    bool   `json:"has_em"`   // Has energy metering
	Modbus   bool   `json:"modbus"`   // Modbus enabled
	ModbusIP string `json:"modbus_ip"` // IP:port for Modbus
}

// Scanner scans for Shelly devices on the network
type Scanner struct {
	timeout time.Duration
	client  *http.Client
}

// NewScanner creates a new Shelly scanner
func NewScanner() *Scanner {
	return &Scanner{
		timeout: 2 * time.Second,
		client: &http.Client{
			Timeout: 2 * time.Second,
		},
	}
}

// ScanNetwork scans a network range for Shelly devices
// network should be like "192.168.1.0/24"
func (s *Scanner) ScanNetwork(network string) ([]ShellyDevice, error) {
	ip, ipNet, err := net.ParseCIDR(network)
	if err != nil {
		// Try as single IP
		ip = net.ParseIP(network)
		if ip == nil {
			return nil, fmt.Errorf("invalid network: %s", network)
		}
		device := s.probeIP(ip.String())
		if device != nil {
			return []ShellyDevice{*device}, nil
		}
		return nil, nil
	}

	var devices []ShellyDevice
	var mu sync.Mutex
	var wg sync.WaitGroup

	// Scan all IPs in range (limit concurrency)
	sem := make(chan struct{}, 50)

	for ip := ip.Mask(ipNet.Mask); ipNet.Contains(ip); incIP(ip) {
		ipStr := ip.String()
		wg.Add(1)
		go func(addr string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			device := s.probeIP(addr)
			if device != nil {
				mu.Lock()
				devices = append(devices, *device)
				mu.Unlock()
			}
		}(ipStr)
	}

	wg.Wait()
	return devices, nil
}

// ScanLocalNetwork scans common local network ranges
func (s *Scanner) ScanLocalNetwork() ([]ShellyDevice, error) {
	// Get local interfaces
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}

	var allDevices []ShellyDevice
	scanned := make(map[string]bool)

	for _, iface := range interfaces {
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}

		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok || ipNet.IP.IsLoopback() || ipNet.IP.To4() == nil {
				continue
			}

			// Derive network from IP and mask
			network := ipNet.IP.Mask(ipNet.Mask).String() + "/" + maskToSize(ipNet.Mask)
			if scanned[network] {
				continue
			}
			scanned[network] = true

			log.Printf("discovery: scanning %s", network)
			devices, err := s.ScanNetwork(network)
			if err != nil {
				log.Printf("discovery: scan error: %v", err)
				continue
			}
			allDevices = append(allDevices, devices...)
		}
	}

	return allDevices, nil
}

// probeIP checks if an IP has a Shelly device
func (s *Scanner) probeIP(ip string) *ShellyDevice {
	// Try Gen2 API first (Shelly Pro EM3 is Gen2)
	device := s.probeGen2(ip)
	if device != nil {
		return device
	}

	// Try Gen1 API
	return s.probeGen1(ip)
}

// probeGen2 checks for Gen2 Shelly device
func (s *Scanner) probeGen2(ip string) *ShellyDevice {
	url := fmt.Sprintf("http://%s/rpc/Shelly.GetDeviceInfo", ip)
	resp, err := s.client.Get(url)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil
	}

	var info struct {
		Name  string `json:"name"`
		ID    string `json:"id"`
		MAC   string `json:"mac"`
		Model string `json:"model"`
		Gen   int    `json:"gen"`
		App   string `json:"app"`
	}
	if err := json.Unmarshal(body, &info); err != nil {
		return nil
	}

	device := &ShellyDevice{
		IP:    ip,
		MAC:   info.MAC,
		Type:  info.App,
		Name:  info.Name,
		Model: info.Model,
		Gen:   info.Gen,
	}

	// Check if it's an EM device
	if strings.Contains(strings.ToLower(info.App), "em") ||
		strings.Contains(strings.ToLower(info.Model), "em") {
		device.HasEM = true

		// Check Modbus status
		modbus := s.checkModbusGen2(ip)
		device.Modbus = modbus
		if modbus {
			device.ModbusIP = ip + ":502"
		}
	}

	log.Printf("discovery: found Shelly %s at %s (Gen%d, EM=%v, Modbus=%v)",
		device.Model, ip, device.Gen, device.HasEM, device.Modbus)

	return device
}

// probeGen1 checks for Gen1 Shelly device
func (s *Scanner) probeGen1(ip string) *ShellyDevice {
	url := fmt.Sprintf("http://%s/shelly", ip)
	resp, err := s.client.Get(url)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil
	}

	var info struct {
		Type string `json:"type"`
		MAC  string `json:"mac"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(body, &info); err != nil {
		return nil
	}

	device := &ShellyDevice{
		IP:    ip,
		MAC:   info.MAC,
		Type:  info.Type,
		Name:  info.Name,
		Model: info.Type,
		Gen:   1,
	}

	// Gen1 EM devices
	if strings.Contains(strings.ToLower(info.Type), "em") {
		device.HasEM = true
	}

	return device
}

// checkModbusGen2 checks if Modbus is enabled on Gen2 device
func (s *Scanner) checkModbusGen2(ip string) bool {
	url := fmt.Sprintf("http://%s/rpc/Modbus.GetConfig", ip)
	resp, err := s.client.Get(url)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return false
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return false
	}

	var cfg struct {
		Enable bool `json:"enable"`
	}
	if err := json.Unmarshal(body, &cfg); err != nil {
		return false
	}

	return cfg.Enable
}

// EnableModbus enables Modbus on a Gen2 Shelly device
func (s *Scanner) EnableModbus(ip string) error {
	url := fmt.Sprintf("http://%s/rpc/Modbus.SetConfig?config={\"enable\":true}", ip)
	resp, err := s.client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("failed to enable modbus: %s", string(body))
	}

	log.Printf("discovery: enabled Modbus on %s", ip)
	return nil
}

// FindEM3Devices returns only EM3 devices suitable for energy monitoring
func (s *Scanner) FindEM3Devices(devices []ShellyDevice) []ShellyDevice {
	var em3Devices []ShellyDevice
	for _, d := range devices {
		if d.HasEM && d.Gen == 2 { // Pro EM3 is Gen2
			em3Devices = append(em3Devices, d)
		}
	}
	return em3Devices
}

func incIP(ip net.IP) {
	for j := len(ip) - 1; j >= 0; j-- {
		ip[j]++
		if ip[j] > 0 {
			break
		}
	}
}

func maskToSize(mask net.IPMask) string {
	ones, _ := mask.Size()
	return fmt.Sprintf("%d", ones)
}
