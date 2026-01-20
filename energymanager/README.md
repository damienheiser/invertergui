# Energy Manager

Lightweight energy management system for OpenWRT routers (tested on GL.iNet GL-SFT1200 Opal).

Monitors grid power via **Shelly Pro EM3** (Modbus TCP), battery status via **Victron BMV/SmartShunt** (VE.Direct), and controls a **Victron MultiPlus** inverter to maintain zero grid power using PID control.

## Features

- **Shelly Pro EM3 Integration** - 3-phase power monitoring via Modbus TCP
- **Victron MultiPlus Control** - Via MK3 USB interface (VE.Bus protocol)
- **Victron BMV Battery Monitor** - Via VE.Direct USB interface
  - Battery voltage, current, power
  - State of charge (SOC)
  - Charged/discharged kWh (lifetime)
  - Time-to-go estimation
  - Alarm status
- **PID Controller** - Keeps grid power at 0W (or configured setpoint)
- **Operating Modes**
  - PID (Zero Grid)
  - Time-of-Use scheduling
  - Solar self-consumption
  - Battery Save (kWh/minutes to sunrise)
- **Web Dashboard** - Minimal, auto-refreshing status page with battery monitor
- **Prometheus Metrics** - `/metrics` endpoint for Grafana integration
- **MQTT Publishing** - Optional publishing to MQTT broker
- **ZeroTier Integration** - LuCI app for remote access
- **Minimal Footprint** - Single static binary, ~7MB for MIPS

## Quick Start

### Prerequisites

- GL.iNet GL-SFT1200 (Opal) or similar OpenWRT router
- Shelly Pro EM3 with Modbus TCP enabled
- Victron MultiPlus with MK3 USB interface
- Victron BMV-700/712 or SmartShunt with VE.Direct cable (optional)
- USB-Serial connections between router and Victron devices

### Enable Modbus on Shelly Pro EM3

In your browser, navigate to:
```
http://<shelly-ip>/rpc/Modbus.SetConfig?config={"enable":true}
```

### Build and Deploy (Recommended)

```bash
# Build deployment package for GL-SFT1200 (MIPS)
./build-firmware.sh
```

This creates `dist/energymanager-1.0.0-gl-sft1200.tar.gz` containing:
- `energymanager` binary (7.1MB MIPS)
- Init script
- UCI config
- LuCI ZeroTier app

### Deploy to Router

```bash
# Copy and extract to router
scp dist/energymanager-1.0.0-gl-sft1200.tar.gz root@<router-ip>:/tmp/
ssh root@<router-ip> "cd / && tar -xzvf /tmp/energymanager-1.0.0-gl-sft1200.tar.gz"

# Install USB serial drivers (required for Victron)
ssh root@<router-ip> "opkg update && opkg install kmod-usb-serial kmod-usb-serial-ftdi kmod-usb-acm"

# Enable and start service
ssh root@<router-ip> "/etc/init.d/energymanager enable && /etc/init.d/energymanager start"
```

### Optional: Install ZeroTier

```bash
ssh root@<router-ip> "opkg install zerotier kmod-tun"
ssh root@<router-ip> "/etc/init.d/zerotier enable && /etc/init.d/zerotier start"
```

### Alternative: Build Complete Firmware (Experimental)

To build a complete firmware image with energymanager pre-installed:

```bash
./docker-build-firmware.sh
```

This uses Docker with the GL.iNet imagebuilder. Note that this can fail due to package availability issues with the Siflower SF19A28 SoC. The deployment package method above is more reliable.

## Configuration

Edit `/etc/config/energymanager`:

```
config main 'main'
    option enabled '1'
    option listen ':8081'
    option shelly_addr ''           # Auto-discover if empty
    option shelly_autoconfig '1'
    option shelly_interval '1s'

config victron 'victron'
    option enabled '1'
    option port '/dev/ttyUSB0'      # VE.Bus via MK2/MK3
    option maxpower '5000'          # Max charge/discharge watts

config bmv 'bmv'
    option enabled '1'
    option port '/dev/ttyUSB1'      # VE.Direct battery monitor

config battery 'battery'
    option capacity '10.0'          # Battery capacity in kWh

config location 'location'
    option latitude '40.7128'
    option longitude '-74.0060'

config pid 'pid'
    option enabled '1'
    option kp '0.8'                 # Proportional gain
    option ki '0.05'                # Integral gain
    option kd '0.1'                 # Derivative gain
    option setpoint '0'             # Target grid power (0 = zero export)

config mode 'mode'
    option default 'pid'            # pid, tou, solar, battery_save

config solar 'solar'
    option export_limit '0'
    option min_soc '20'
    option max_soc '100'

config battery_save 'battery_save'
    option min_soc '20'
    option target_soc_sunrise '30'
    option sunrise_offset_minutes '30'

config mqtt 'mqtt'
    option enabled '0'
    option broker 'tcp://localhost:1883'
    option topic 'energy/status'

config auth 'auth'
    option username 'admin'        # Web UI login (default: admin)
    option password 'energy'       # Web UI password (default: energy)
```

## Command Line Options

```
-listen string
      HTTP listen address (default ":8081")
-shelly.addr string
      Shelly Pro EM3 address (host:port), auto-discover if empty
-shelly.autoconfig
      Auto-discover and configure Shelly (default true)
-shelly.interval duration
      Shelly poll interval (default 1s)
-victron.enabled
      Enable Victron inverter (default true)
-victron.port string
      Victron VE.Bus serial port (default "/dev/ttyUSB0")
-victron.maxpower float
      Max inverter power in watts (default 5000)
-bmv.enabled
      Enable Victron BMV battery monitor (default true)
-bmv.port string
      BMV VE.Direct serial port (default "/dev/ttyUSB1")
-battery.capacity float
      Battery capacity in kWh (default 10.0)
-latitude float
      Latitude for sunrise calculation
-longitude float
      Longitude for sunrise calculation
-pid.enabled
      Enable PID controller (default true)
-pid.kp float
      PID proportional gain (default 0.8)
-pid.ki float
      PID integral gain (default 0.05)
-pid.kd float
      PID derivative gain (default 0.1)
-pid.setpoint float
      Target grid power in watts (default 0)
-mode string
      Default operating mode (default "pid")
-mqtt.enabled
      Enable MQTT publishing
-mqtt.broker string
      MQTT broker address (default "tcp://localhost:1883")
-mqtt.topic string
      MQTT topic (default "energy/status")
-auth.username string
      Admin username for protected pages (default "admin")
-auth.password string
      Admin password for protected pages (default "energy")
```

## Web Endpoints

| Endpoint | Auth | Description |
|----------|------|-------------|
| `/` | No | Dashboard with grid, inverter, battery monitor, and mode status |
| `/api/status` | No | JSON API with all current data |
| `/metrics` | No | Prometheus metrics |
| `/config` | **Yes** | Full configuration UI |
| `/api/config` | **Yes** | GET/POST configuration |
| `/api/mode` | **Yes** | GET/POST operating mode |
| `/api/schedule` | **Yes** | GET/POST mode schedule |
| `/api/override` | **Yes** | POST manual setpoint override |
| `/api/discover` | **Yes** | Network discovery for Shelly devices |

## Authentication

Protected endpoints require HTTP Basic Authentication using your **router's root password** (same as LuCI/SSH).

**Login credentials:**
- **Username:** `root`
- **Password:** *your router's root password*

This means you only need to remember one set of credentials for both LuCI and Energy Manager.

**Fallback:** If system authentication is unavailable (e.g., testing on non-OpenWRT systems), you can configure fallback credentials:
```
config auth 'auth'
    option username 'admin'
    option password 'yourpassword'
```

## Prometheus Metrics

Battery monitor metrics available at `/metrics`:

| Metric | Description |
|--------|-------------|
| `battery_voltage_volts` | Battery voltage from BMV |
| `battery_current_amps` | Battery current (negative = discharging) |
| `battery_power_watts` | Battery instantaneous power |
| `battery_soc_percent` | State of charge 0-100% |
| `battery_time_to_go_minutes` | Minutes until empty (-1 = infinite) |
| `battery_charged_kwh_total` | Total energy charged (lifetime) |
| `battery_discharged_kwh_total` | Total energy discharged (lifetime) |
| `battery_charge_cycles_total` | Number of charge cycles |
| `battery_min_voltage_volts` | Minimum recorded voltage |
| `battery_max_voltage_volts` | Maximum recorded voltage |
| `battery_temperature_celsius` | Battery temperature (if available) |

## Architecture

```
┌─────────────────────────────────────────────────────────────────┐
│                        Energy Manager                            │
├─────────────────────────────────────────────────────────────────┤
│                                                                  │
│  ┌───────────────┐  ┌───────────────┐  ┌───────────────┐       │
│  │ Shelly EM3    │  │ Victron MK3   │  │ Victron BMV   │       │
│  │ (Modbus TCP)  │  │ (VE.Bus USB)  │  │ (VE.Direct)   │       │
│  └───────┬───────┘  └───────┬───────┘  └───────┬───────┘       │
│          │                  │                  │                │
│          ▼                  ▼                  ▼                │
│  ┌─────────────────────────────────────────────────────────┐   │
│  │                  Control Loop (1 Hz)                     │   │
│  │  1. Read grid power from Shelly EM3                     │   │
│  │  2. Read battery data from BMV (voltage, current, SOC)  │   │
│  │  3. Read inverter status from MultiPlus                 │   │
│  │  4. Calculate mode setpoint (PID/TOU/Solar/BatSave)     │   │
│  │  5. Run PID controller                                   │   │
│  │  6. Send command to inverter                             │   │
│  │  7. Broadcast to subscribers                             │   │
│  └───────────────────────┬─────────────────────────────────┘   │
│                          │                                      │
│         ┌────────────────┼────────────────┐                    │
│         ▼                ▼                ▼                    │
│   ┌─────────┐      ┌──────────┐     ┌──────────┐              │
│   │ Web UI  │      │Prometheus│     │   MQTT   │              │
│   │  :8081  │      │ /metrics │     │ publish  │              │
│   └─────────┘      └──────────┘     └──────────┘              │
│                                                                  │
└─────────────────────────────────────────────────────────────────┘
```

## Hardware Connections

```
GL-SFT1200 USB Port 1 ──USB──> Victron MK3-USB ──VE.Bus──> MultiPlus
GL-SFT1200 USB Port 2 ──USB──> VE.Direct Cable ──────────> BMV/SmartShunt

Shelly Pro EM3 ──Ethernet──> Local Network ──Modbus TCP:502──> GL-SFT1200
```

## VE.Direct Supported Devices

The BMV driver supports all Victron products using the VE.Direct text protocol:
- BMV-700, BMV-702, BMV-700H
- BMV-712 Smart, BMV-710H Smart
- SmartShunt 500A/50mV, 1000A/50mV, 2000A/50mV

## PID Tuning

The default PID values work well for most systems:

- **Kp = 0.8**: Responds to 80% of the error immediately
- **Ki = 0.05**: Slowly eliminates steady-state error
- **Kd = 0.1**: Dampens oscillations

If you experience oscillation, reduce Kp and Ki. If response is too slow, increase Kp.

## Operating Modes

| Mode | Description |
|------|-------------|
| **Scheduled** | Automatic mode switching based on time schedule (supports sunrise/sunset-relative times) |
| **PID** | Zero-grid mode - maintains grid power at setpoint (default 0W) |
| **TOU** | Time-of-Use - charge during off-peak, discharge during peak |
| **Solar** | Solar-first mode with zero-export option - uses solar to power loads before battery |
| **Battery Save** | Discharge overnight: kWh remaining ÷ minutes to sunrise = discharge rate |

### Solar Mode Features

Solar mode prioritizes using solar energy efficiently:

1. **Solar-First**: Solar powers house loads before being used to charge batteries
2. **Zero Export**: When enabled (default), NEVER exports excess power to the grid - all excess charges battery
3. **Self-Consumption**: Uses battery to power loads when solar is insufficient
4. **SOC Limits**: Respects Min/Max SOC settings to protect battery

### Mode Schedule

The **Scheduled** mode allows automatic mode switching based on time. Supports:

- **Absolute times**: Fixed hours (e.g., 15:00)
- **Sunrise-relative**: Time relative to sunrise (e.g., sunrise + 30min)
- **Sunset-relative**: Time relative to sunset (e.g., sunset - 60min)
- **Priority**: Higher priority periods take precedence when overlapping

Example schedule (Midnight→Sunrise→3PM→Midnight):
```
Night (00:00 - Sunrise): Battery Save mode
Day (Sunrise - 15:00): Solar mode
Evening (15:00 - 24:00): TOU mode
```

Configure via the web UI at `/config` or use the "Quick Setup" presets.

## Project Structure

```
energymanager/
├── cmd/energymanager/main.go    # Application entry point
├── core/                        # Data types and pub-sub hub
├── serial/                      # Cross-platform serial port (no external deps)
├── shelly/                      # Modbus TCP client + EM3 driver
├── vebus/                       # Victron VE.Bus protocol (MultiPlus)
├── vedirect/                    # Victron VE.Direct protocol (BMV)
├── pid/                         # PID controller
├── modes/                       # Operating mode logic
├── discovery/                   # Network device discovery
├── webui/                       # Web dashboard and API
├── prometheus/                  # Metrics endpoint
├── mqtt/                        # MQTT publisher
├── openwrt/                     # OpenWRT packaging files
├── docker/                      # Docker firmware build environment
├── build.sh                     # Quick build script
├── build-firmware.sh            # Full deployment package builder
└── docker-build-firmware.sh     # Docker-based firmware builder
```

## License

BSD-3-Clause (same as original invertergui project)
