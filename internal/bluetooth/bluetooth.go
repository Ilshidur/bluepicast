package bluetooth

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
)

// Device represents a discovered Bluetooth device
type Device struct {
	Address   string `json:"address"`
	Name      string `json:"name"`
	Paired    bool   `json:"paired"`
	Connected bool   `json:"connected"`
	Trusted   bool   `json:"trusted"`
	RSSI      int16  `json:"rssi"`
	Icon      string `json:"icon"`
}

// Adapter manages Bluetooth operations via BlueZ D-Bus API
type Adapter struct {
	conn        *dbus.Conn
	adapterPath dbus.ObjectPath
	mu          sync.RWMutex
	devices     map[string]*Device
	onChange    func(devices []*Device)
	onConnect   func(device *Device)
	scanning    bool
	stopSignals chan struct{}
}

const (
	bluezService        = "org.bluez"
	bluezAdapterIface   = "org.bluez.Adapter1"
	bluezDeviceIface    = "org.bluez.Device1"
	dbusPropertiesIface = "org.freedesktop.DBus.Properties"
	dbusObjectManager   = "org.freedesktop.DBus.ObjectManager"
)

// NewAdapter creates a new Bluetooth adapter manager
func NewAdapter() (*Adapter, error) {
	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		return nil, fmt.Errorf("failed to connect to system bus: %w", err)
	}

	adapter := &Adapter{
		conn:        conn,
		devices:     make(map[string]*Device),
		stopSignals: make(chan struct{}),
	}

	// Find the default adapter (usually hci0)
	adapterPath, err := adapter.findAdapter()
	if err != nil {
		conn.Close()
		return nil, err
	}
	adapter.adapterPath = adapterPath

	// Ensure the adapter is powered on
	if err := adapter.ensurePoweredOn(); err != nil {
		log.Printf("Warning: Failed to power on adapter: %v", err)
	}

	// Set up signal handling for device changes
	if err := adapter.setupSignals(); err != nil {
		conn.Close()
		return nil, err
	}

	// Load existing paired/connected devices at startup
	adapter.loadExistingDevices()

	return adapter, nil
}

// SetOnChange sets the callback for device list changes
func (a *Adapter) SetOnChange(fn func(devices []*Device)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.onChange = fn
}

// SetOnConnect sets the callback for when a device connects
func (a *Adapter) SetOnConnect(fn func(device *Device)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.onConnect = fn
}

// ensurePoweredOn makes sure the Bluetooth adapter is powered on
func (a *Adapter) ensurePoweredOn() error {
	adapter := a.conn.Object(bluezService, a.adapterPath)

	// Check current power state
	variant, err := adapter.GetProperty(bluezAdapterIface + ".Powered")
	if err != nil {
		return fmt.Errorf("failed to get power state: %w", err)
	}

	powered, ok := variant.Value().(bool)
	if ok && powered {
		log.Println("Bluetooth adapter is already powered on")
		return nil
	}

	// Power on the adapter
	log.Println("Powering on Bluetooth adapter...")
	call := adapter.Call(dbusPropertiesIface+".Set", 0, bluezAdapterIface, "Powered", dbus.MakeVariant(true))
	if call.Err == nil {
		log.Println("Bluetooth adapter powered on successfully")
		return nil
	}

	// If initial attempt failed, try to unblock Bluetooth via rfkill and retry once
	log.Printf("Initial attempt to power on Bluetooth adapter failed: %v", call.Err)
	if err := tryUnblockBluetoothRfkill(); err != nil {
		log.Printf("rfkill unblock bluetooth failed or not available: %v", err)
		return fmt.Errorf("failed to power on adapter: %w", call.Err)
	}

	log.Println("Retrying to power on Bluetooth adapter after rfkill unblock...")
	call = adapter.Call(dbusPropertiesIface+".Set", 0, bluezAdapterIface, "Powered", dbus.MakeVariant(true))
	if call.Err != nil {
		return fmt.Errorf("failed to power on adapter after rfkill unblock: %w", call.Err)
	}

	log.Println("Bluetooth adapter powered on successfully after rfkill unblock")
	return nil
}

// tryUnblockBluetoothRfkill attempts to unblock Bluetooth via rfkill if it is soft-blocked.
// It is safe to call even if rfkill is not installed or Bluetooth is not blocked.
func tryUnblockBluetoothRfkill() error {
	// Check if rfkill command exists
	if _, err := exec.LookPath("rfkill"); err != nil {
		return fmt.Errorf("rfkill not found")
	}

	// Check rfkill state for Bluetooth
	cmd := exec.Command("rfkill", "list", "bluetooth")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("rfkill list bluetooth failed: %w (output: %s)", err, strings.TrimSpace(string(output)))
	}

	// If not soft-blocked, nothing to do
	if !strings.Contains(strings.ToLower(string(output)), "soft blocked: yes") {
		return nil
	}

	log.Println("Bluetooth is soft-blocked via rfkill. Attempting to unblock...")
	cmd = exec.Command("rfkill", "unblock", "bluetooth")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("rfkill unblock bluetooth failed: %w", err)
	}

	return nil
}

func (a *Adapter) findAdapter() (dbus.ObjectPath, error) {
	obj := a.conn.Object(bluezService, "/")
	var result map[dbus.ObjectPath]map[string]map[string]dbus.Variant

	err := obj.Call(dbusObjectManager+".GetManagedObjects", 0).Store(&result)
	if err != nil {
		return "", fmt.Errorf("failed to get managed objects: %w", err)
	}

	for path, ifaces := range result {
		if _, ok := ifaces[bluezAdapterIface]; ok {
			return path, nil
		}
	}

	return "", fmt.Errorf("no Bluetooth adapter found")
}

func (a *Adapter) setupSignals() error {
	if err := a.conn.AddMatchSignal(
		dbus.WithMatchInterface(dbusObjectManager),
	); err != nil {
		return err
	}

	if err := a.conn.AddMatchSignal(
		dbus.WithMatchInterface(dbusPropertiesIface),
		dbus.WithMatchMember("PropertiesChanged"),
	); err != nil {
		return err
	}

	signals := make(chan *dbus.Signal, 10)
	a.conn.Signal(signals)

	go func() {
		for {
			select {
			case <-a.stopSignals:
				a.conn.RemoveSignal(signals)
				return
			case signal, ok := <-signals:
				if !ok {
					return
				}
				a.handleSignal(signal)
			}
		}
	}()

	return nil
}

func (a *Adapter) handleSignal(signal *dbus.Signal) {
	switch signal.Name {
	case dbusObjectManager + ".InterfacesAdded":
		if len(signal.Body) >= 2 {
			path, ok := signal.Body[0].(dbus.ObjectPath)
			if !ok {
				return
			}
			ifaces, ok := signal.Body[1].(map[string]map[string]dbus.Variant)
			if !ok {
				return
			}
			if props, ok := ifaces[bluezDeviceIface]; ok {
				a.updateDevice(path, props)
			}
		}
	case dbusObjectManager + ".InterfacesRemoved":
		if len(signal.Body) >= 2 {
			path, ok := signal.Body[0].(dbus.ObjectPath)
			if !ok {
				return
			}
			a.removeDevice(path)
		}
	case dbusPropertiesIface + ".PropertiesChanged":
		if len(signal.Body) >= 2 {
			iface, ok := signal.Body[0].(string)
			if !ok || iface != bluezDeviceIface {
				return
			}
			props, ok := signal.Body[1].(map[string]dbus.Variant)
			if !ok {
				return
			}
			a.updateDevice(signal.Path, props)
		}
	}
}

func (a *Adapter) updateDevice(path dbus.ObjectPath, props map[string]dbus.Variant) {
	pathStr := string(path)
	if !strings.HasPrefix(pathStr, string(a.adapterPath)+"/dev_") {
		return
	}

	a.mu.Lock()

	device, exists := a.devices[pathStr]
	if !exists {
		device = &Device{}
		a.devices[pathStr] = device
	}

	// Track previous connection state to detect new connections
	wasConnected := device.Connected

	for key, val := range props {
		switch key {
		case "Address":
			if v, ok := val.Value().(string); ok {
				device.Address = v
			}
		case "Name", "Alias":
			if v, ok := val.Value().(string); ok && v != "" {
				device.Name = v
			}
		case "Paired":
			if v, ok := val.Value().(bool); ok {
				device.Paired = v
			}
		case "Connected":
			if v, ok := val.Value().(bool); ok {
				device.Connected = v
			}
		case "Trusted":
			if v, ok := val.Value().(bool); ok {
				device.Trusted = v
			}
		case "RSSI":
			if v, ok := val.Value().(int16); ok {
				device.RSSI = v
			}
		case "Icon":
			if v, ok := val.Value().(string); ok {
				device.Icon = v
			}
		}
	}

	// Check if device just connected (was not connected before, now is connected)
	justConnected := !wasConnected && device.Connected
	onConnectCallback := a.onConnect
	// Create a copy for the callback to avoid race conditions since the
	// original device struct may be modified by subsequent D-Bus signals
	deviceCopy := *device

	a.mu.Unlock()

	if a.onChange != nil {
		go a.onChange(a.GetDevices())
	}

	// Trigger onConnect callback if the device just connected
	if justConnected && onConnectCallback != nil {
		go onConnectCallback(&deviceCopy)
	}
}

func (a *Adapter) removeDevice(path dbus.ObjectPath) {
	a.mu.Lock()
	_, exists := a.devices[string(path)]
	if exists {
		delete(a.devices, string(path))
	}
	onChange := a.onChange
	a.mu.Unlock()

	// Only trigger onChange if the device was actually in our map
	if exists && onChange != nil {
		go onChange(a.GetDevices())
	}
}

// GetDevices returns all discovered devices
func (a *Adapter) GetDevices() []*Device {
	a.mu.RLock()
	defer a.mu.RUnlock()

	devices := make([]*Device, 0, len(a.devices))
	for _, d := range a.devices {
		devices = append(devices, d)
	}
	return devices
}

// GetPairedDevices returns only paired or connected devices
func (a *Adapter) GetPairedDevices() []*Device {
	a.mu.RLock()
	defer a.mu.RUnlock()

	devices := make([]*Device, 0)
	for _, d := range a.devices {
		if d.Paired || d.Connected {
			devices = append(devices, d)
		}
	}
	return devices
}

// StartDiscovery begins scanning for Bluetooth devices
func (a *Adapter) StartDiscovery(ctx context.Context) error {
	a.mu.Lock()
	if a.scanning {
		a.mu.Unlock()
		log.Println("Discovery already in progress")
		return nil
	}
	a.scanning = true
	a.mu.Unlock()

	// Ensure adapter is powered on before starting discovery
	// This is intentionally called again (also at init) because the adapter
	// might have been powered off by rfkill or another process since startup
	if err := a.ensurePoweredOn(); err != nil {
		a.mu.Lock()
		a.scanning = false
		a.mu.Unlock()
		log.Printf("Failed to power on adapter: %v", err)
		return fmt.Errorf("failed to power on adapter: %w", err)
	}

	adapter := a.conn.Object(bluezService, a.adapterPath)

	// Refresh device list to catch any devices registered by BlueZ since startup
	a.loadExistingDevices()

	log.Println("Starting Bluetooth discovery...")
	call := adapter.Call(bluezAdapterIface+".StartDiscovery", 0)
	if call.Err != nil {
		a.mu.Lock()
		a.scanning = false
		a.mu.Unlock()
		log.Printf("Failed to start discovery: %v", call.Err)
		return fmt.Errorf("failed to start discovery: %w", call.Err)
	}

	log.Println("Bluetooth discovery started successfully")
	return nil
}

// StopDiscovery stops scanning for Bluetooth devices
func (a *Adapter) StopDiscovery() error {
	a.mu.Lock()
	if !a.scanning {
		a.mu.Unlock()
		log.Println("Discovery not in progress")
		return nil
	}
	a.mu.Unlock()

	log.Println("Stopping Bluetooth discovery...")
	adapter := a.conn.Object(bluezService, a.adapterPath)
	call := adapter.Call(bluezAdapterIface+".StopDiscovery", 0)

	a.mu.Lock()
	a.scanning = false
	a.mu.Unlock()

	if call.Err != nil {
		// Ignore error if discovery was not started - check for D-Bus error
		// BlueZ returns "org.bluez.Error.Failed" with message "No discovery started"
		var dbusErr dbus.Error
		if errors.As(call.Err, &dbusErr) {
			if dbusErr.Name == "org.bluez.Error.Failed" || dbusErr.Name == "org.bluez.Error.NotReady" {
				// These are expected errors, ignore them
				log.Println("Discovery stopped (was not active)")
				return nil
			}
		}
		log.Printf("Failed to stop discovery: %v", call.Err)
		return fmt.Errorf("failed to stop discovery: %w", call.Err)
	}

	log.Println("Bluetooth discovery stopped successfully")
	return nil
}

// IsScanning returns whether discovery is active
func (a *Adapter) IsScanning() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.scanning
}

func (a *Adapter) loadExistingDevices() {
	obj := a.conn.Object(bluezService, "/")
	var result map[dbus.ObjectPath]map[string]map[string]dbus.Variant

	err := obj.Call(dbusObjectManager+".GetManagedObjects", 0).Store(&result)
	if err != nil {
		return
	}

	for path, ifaces := range result {
		if props, ok := ifaces[bluezDeviceIface]; ok {
			a.updateDevice(path, props)
		}
	}
}

// refreshDeviceProperties fetches all properties for a specific device from D-Bus
// and updates the internal device state
func (a *Adapter) refreshDeviceProperties(devicePath string) {
	device := a.conn.Object(bluezService, dbus.ObjectPath(devicePath))

	var props map[string]dbus.Variant
	err := device.Call(dbusPropertiesIface+".GetAll", 0, bluezDeviceIface).Store(&props)
	if err != nil {
		log.Printf("Failed to refresh device properties for %s: %v", devicePath, err)
		return
	}

	a.updateDevice(dbus.ObjectPath(devicePath), props)
	log.Printf("Refreshed properties for device: %s", devicePath)
}

// Trust sets a device as trusted
func (a *Adapter) Trust(address string) error {
	log.Printf("Trusting device: %s", address)
	devicePath := a.getDevicePath(address)
	if devicePath == "" {
		log.Printf("Device not found: %s", address)
		return fmt.Errorf("device not found: %s", address)
	}

	device := a.conn.Object(bluezService, dbus.ObjectPath(devicePath))
	call := device.Call(dbusPropertiesIface+".Set", 0, bluezDeviceIface, "Trusted", dbus.MakeVariant(true))
	if call.Err != nil {
		log.Printf("Failed to trust %s: %v", address, call.Err)
		return fmt.Errorf("failed to trust: %w", call.Err)
	}

	log.Printf("Successfully trusted device: %s", address)

	// Refresh device properties to ensure we have the updated state
	a.refreshDeviceProperties(devicePath)

	return nil
}

// Pair initiates pairing with a device
func (a *Adapter) Pair(address string) error {
	log.Printf("Pairing with device: %s", address)
	devicePath := a.getDevicePath(address)
	if devicePath == "" {
		log.Printf("Device not found: %s", address)
		return fmt.Errorf("device not found: %s", address)
	}

	device := a.conn.Object(bluezService, dbus.ObjectPath(devicePath))
	call := device.Call(bluezDeviceIface+".Pair", 0)
	if call.Err != nil {
		log.Printf("Failed to pair with %s: %v", address, call.Err)
		return fmt.Errorf("failed to pair: %w", call.Err)
	}

	log.Printf("Successfully paired with device: %s", address)

	// Refresh device properties to ensure we have the updated state
	a.refreshDeviceProperties(devicePath)

	return nil
}

// Connect connects to a paired device and trusts it
func (a *Adapter) Connect(address string) error {
	log.Printf("Connecting to device: %s", address)
	devicePath := a.getDevicePath(address)
	if devicePath == "" {
		log.Printf("Device not found: %s", address)
		return fmt.Errorf("device not found: %s", address)
	}

	// Trust the device before connecting
	if err := a.Trust(address); err != nil {
		log.Printf("Warning: Failed to trust device before connecting: %v", err)
		// Continue with connection even if trust fails
	}

	device := a.conn.Object(bluezService, dbus.ObjectPath(devicePath))
	call := device.Call(bluezDeviceIface+".Connect", 0)
	if call.Err != nil {
		log.Printf("Failed to connect to %s: %v", address, call.Err)
		return fmt.Errorf("failed to connect: %w", call.Err)
	}

	log.Printf("Successfully connected to device: %s", address)

	// Refresh device properties to ensure we have the updated state
	a.refreshDeviceProperties(devicePath)

	return nil
}

// Disconnect disconnects from a device
func (a *Adapter) Disconnect(address string) error {
	log.Printf("Disconnecting from device: %s", address)
	devicePath := a.getDevicePath(address)
	if devicePath == "" {
		log.Printf("Device not found: %s", address)
		return fmt.Errorf("device not found: %s", address)
	}

	device := a.conn.Object(bluezService, dbus.ObjectPath(devicePath))
	call := device.Call(bluezDeviceIface+".Disconnect", 0)
	if call.Err != nil {
		log.Printf("Failed to disconnect from %s: %v", address, call.Err)
		return fmt.Errorf("failed to disconnect: %w", call.Err)
	}

	log.Printf("Successfully disconnected from device: %s", address)

	// Refresh device properties to ensure we have the updated state
	a.refreshDeviceProperties(devicePath)

	return nil
}

// Remove unpairs and removes a device
func (a *Adapter) Remove(address string) error {
	log.Printf("Removing device: %s", address)
	devicePath := a.getDevicePath(address)
	if devicePath == "" {
		log.Printf("Device not found: %s", address)
		return fmt.Errorf("device not found: %s", address)
	}

	adapter := a.conn.Object(bluezService, a.adapterPath)
	call := adapter.Call(bluezAdapterIface+".RemoveDevice", 0, dbus.ObjectPath(devicePath))
	if call.Err != nil {
		log.Printf("Failed to remove device %s: %v", address, call.Err)
		return fmt.Errorf("failed to remove device: %w", call.Err)
	}

	// Manually remove the device from the internal map and trigger onChange
	// This ensures the UI updates immediately without waiting for the D-Bus signal
	// Using a check to avoid duplicate onChange calls if the D-Bus signal arrives first
	a.mu.Lock()
	_, exists := a.devices[devicePath]
	if exists {
		delete(a.devices, devicePath)
	}
	onChange := a.onChange
	a.mu.Unlock()

	if exists && onChange != nil {
		go onChange(a.GetDevices())
	}

	log.Printf("Successfully removed device: %s", address)
	return nil
}

func (a *Adapter) getDevicePath(address string) string {
	// Convert address format: XX:XX:XX:XX:XX:XX -> dev_XX_XX_XX_XX_XX_XX
	devAddr := "dev_" + strings.ReplaceAll(address, ":", "_")
	return string(a.adapterPath) + "/" + devAddr
}

// Close cleans up resources
func (a *Adapter) Close() error {
	log.Println("Closing Bluetooth adapter...")
	a.StopDiscovery()

	// Stop the signal handling goroutine
	close(a.stopSignals)

	err := a.conn.Close()
	log.Println("Bluetooth adapter closed")
	return err
}

// ScanFor scans for devices for the specified duration
func (a *Adapter) ScanFor(ctx context.Context, duration time.Duration) error {
	if err := a.StartDiscovery(ctx); err != nil {
		return err
	}

	select {
	case <-time.After(duration):
	case <-ctx.Done():
	}

	return a.StopDiscovery()
}
