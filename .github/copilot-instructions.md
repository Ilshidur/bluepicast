# BluePiCast Development Guide

## Project Overview

BluePiCast is a Go-based Bluetooth device manager and audio streamer for Raspberry Pi with web UI control. The system integrates BlueZ (via D-Bus), BlueALSA for audio routing, and optionally Snapcast for multi-room audio.

**Architecture:** The application follows a clean layered architecture:
- `cmd/server` - Entry point, coordinates all components
- `internal/bluetooth` - BlueZ D-Bus integration for device discovery/pairing/connection
- `internal/audio` - ALSA configuration management (writes `.asoundrc`)
- `internal/snapcast` - Systemd service management for Snapclient
- `internal/web` - HTTP/WebSocket server with embedded static files

**Key Data Flow:**
1. BlueZ D-Bus signals → `bluetooth.Adapter` → callbacks → `web.Server` → WebSocket broadcast
2. Web UI commands → WebSocket → `web.Server` handlers → component actions
3. Device connections trigger automatic audio routing (when enabled) and Snapclient restarts

## Critical Development Patterns

### D-Bus Integration
All Bluetooth operations use D-Bus to communicate with BlueZ. The `bluetooth.Adapter` maintains a persistent system bus connection and subscribes to signals for device state changes:

```go
// Example: Device state changes come via D-Bus signals
func (a *Adapter) setupSignals() error {
    // Listens to org.freedesktop.DBus.ObjectManager signals
    // InterfacesAdded/InterfacesRemoved for device discovery
    // PropertiesChanged for connection/pairing state updates
}
```

**Important:** All D-Bus operations require root privileges. When testing, use `sudo`.

### WebSocket Message Protocol
The web UI communicates using JSON messages with `type` and `payload` fields. See `internal/web/server.go` for the complete message type enum (`MsgType*` constants). All payloads are `json.RawMessage` that get unmarshaled based on type.

**Pattern:** Each message type has a corresponding handler in `handleMessage()` that typically:
1. Unmarshals payload to specific struct
2. Launches goroutine for async operation
3. Broadcasts status/results back to all connected clients

### ALSA Audio Routing
Audio routing works by writing `.asoundrc` configuration files. The `audio.Manager` provides:
- `SetDefaultDevice(address)` - Writes bluealsa config for specific MAC address
- `GetCurrentDevice()` - Parses `.asoundrc` to find current device
- Input validation using regex to prevent MAC address injection

**Key constraint:** ALSA routing only works with `player=alsa` and `soundcard=bluealsa` in Snapclient config.

### Snapclient Service Management
The `snapcast.Manager` manages **user-level systemd services** (not system services) using `systemctl --user` commands. Configuration is stored in `~/.config/snapclient/options` (not `/etc/default/snapclient`).

**Critical:** The service must run as a user service for non-root access. See `MigrateToUserService()` for migration logic from system to user service.

## Build & Test Workflows

### Local Development (Linux with BlueZ required)
```bash
# Hot reload during development
go install github.com/air-verse/air@latest
sudo $(go env GOPATH)/bin/air

# Or direct run
sudo go run ./cmd/server --port 8080 --enable-systemd-snapclient
# Or with HTTPS
sudo go run ./cmd/server --port 8443 --enable-systemd-snapclient --https
```

### Cross-Compilation for Raspberry Pi
```bash
# Pi 3/4 (64-bit) - most common
GOOS=linux GOARCH=arm64 go build -o bluepicast-arm64 ./cmd/server

# Pi 3/4 (32-bit)
GOOS=linux GOARCH=arm GOARM=7 go build -o bluepicast-armv7 ./cmd/server
```

### Testing
Run unit tests with:
```bash
go test ./...
```

Tests focus on parsing logic (e.g., `snapcast_test.go` validates Snapclient options parsing).

**Note:** Integration tests requiring BlueZ/D-Bus are not automated - test manually on target hardware.

## Project-Specific Conventions

### Embedded Static Files
The web UI is embedded using `//go:embed` in `internal/web/server.go`:
```go
//go:embed static/*
var staticFiles embed.FS
```
Changes to HTML/CSS/JS require rebuilding the binary. No separate asset compilation step needed.

### Concurrency & Locking
- `bluetooth.Adapter` uses `sync.RWMutex` for device map access
- `web.Server` uses separate mutexes for client connections and ALSA config
- WebSocket client writes use per-client mutex to prevent concurrent write panics
- Long-running operations (scan, pair, connect) run in goroutines

### Error Handling Pattern
Components return errors up the stack. The web layer converts errors to WebSocket error messages:
```go
if err := s.adapter.Pair(address); err != nil {
    s.sendError(c, fmt.Sprintf("Failed to pair: %v", err))
    return
}
```

### Security Considerations
- MAC address validation via regex before any system operations (`macAddressPattern` in `audio/audio.go`)
- WebSocket origin checking allows local network access (permissive for IoT use case)
- Root privileges required for BlueZ D-Bus access

## Pi-gen Image Building

The `pi-gen-config/` directory contains configuration for building custom Raspberry Pi OS images with BluePiCast pre-installed. Uses the official Raspberry Pi [pi-gen](https://github.com/RPi-Distro/pi-gen) tool.

**Custom stage:** `stage-bluepicast/` installs bluez-alsa, snapclient, and BluePiCast service:
- `00-install-packages/` - System dependencies
- `01-install-bluealsa/` - Compiles bluez-alsa from source
- `02-install-snapclient/` - Downloads/installs Snapclient binary
- `03-install-bluepicast/` - Fetches latest release from GitHub
- `04-configure-services/` - Sets up systemd services

Build images using Docker: `cd pi-gen && ./build-docker.sh`

## Common Tasks

**Add new WebSocket message type:**
1. Add constant to `MessageType` enum in `internal/web/server.go`
2. Define payload struct if needed
3. Add case to `handleMessage()` switch statement
4. Update web UI JavaScript to handle new message type

**Modify Bluetooth device properties:**
1. Update `Device` struct in `internal/bluetooth/bluetooth.go`
2. Ensure D-Bus property is read in `deviceFromObject()` or signal handlers
3. Properties are automatically broadcast via existing WebSocket flow

**Change ALSA routing logic:**
- Modify `audio.Manager` methods
- Update `.asoundrc` template format in `SetDefaultDevice()`
- Check `handleDeviceConnected()` in web server for auto-routing trigger points

## Known Constraints

- Requires Linux with BlueZ stack (Raspberry Pi OS recommended)
- Must run as root for D-Bus system bus access
- Snapclient integration only works with user-level systemd services
- ALSA routing requires bluez-alsa (bluealsa) installation
- WebSocket clients receive full device list on every update (no delta updates)

## Debugging Tips

**Check BlueZ status:** `sudo systemctl status bluetooth`
**Monitor D-Bus traffic:** `dbus-monitor --system`
**View Snapclient logs:** `journalctl --user -u snapclient -f`
**Check ALSA devices:** `aplay -l` and inspect `~/.asoundrc`
