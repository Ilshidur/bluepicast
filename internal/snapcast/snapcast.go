package snapcast

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// Compiled regex for parsing volume percentage from amixer output
var volumeRegex = regexp.MustCompile(`\d+`)

// Manager handles Snapclient operations
type Manager struct {
	enabled        bool
	executablePath string
	configPath     string
	mu             sync.RWMutex
}

// Config represents the Snapclient configuration
type Config struct {
	Host                string `json:"host"`
	InstanceID          string `json:"instanceId"`
	Player              string `json:"player"`
	Soundcard           string `json:"soundcard"`
	Volume              int    `json:"volume"`              // ALSA volume percentage (0-100), only used when player is "alsa"
	SoundcardAvailable  bool   `json:"soundcardAvailable"`  // Indicates if the soundcard is available in the system (checked via aplay -l)
}

// Status represents the current state of the Snapclient service
type Status struct {
	Running            bool   `json:"running"`
	Failed             bool   `json:"failed"`
	Version            string `json:"version"`
	Config             Config `json:"config"`
	IsSystemService    bool   `json:"isSystemService"`    // True only if system service is actively running/enabled
	UserServiceEnabled bool   `json:"userServiceEnabled"` // True if user service is enabled (even if not running)
}

// MigrationResult contains the result of migration attempt
type MigrationResult struct {
	Success     bool     `json:"success"`
	ManualSteps []string `json:"manualSteps,omitempty"`
	Error       string   `json:"error,omitempty"`
}

// Player represents an available audio player
type Player struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Available   bool   `json:"available"` // Indicates if the device is available in aplay -l
}

const (
	defaultExecutablePath = "/usr/bin/snapclient"
	defaultPlayer         = "bluealsa"
	systemConfigPath      = "/etc/default/snapclient"
	logChannelBufferSize  = 100 // Buffer size for log streaming channel
	defaultLogLines       = 100 // Number of initial log lines to fetch
)

// getRealUser returns the actual user (not root) who should own the user service
// Returns username, uid, and home directory
func getRealUser() (string, string, string, error) {
	// Check SUDO_USER first (set when running via sudo)
	sudoUser := os.Getenv("SUDO_USER")
	if sudoUser != "" && sudoUser != "root" {
		// Get user info
		cmd := exec.Command("id", "-u", sudoUser)
		output, err := cmd.Output()
		if err == nil {
			uid := strings.TrimSpace(string(output))
			cmd = exec.Command("getent", "passwd", sudoUser)
			output, err = cmd.Output()
			if err == nil {
				parts := strings.Split(string(output), ":")
				if len(parts) >= 6 {
					return sudoUser, uid, parts[5], nil
				}
			}
		}
	}

	// Fallback: find first non-root user with UID >= 1000
	cmd := exec.Command("getent", "passwd")
	output, err := cmd.Output()
	if err != nil {
		return "", "", "", fmt.Errorf("failed to get passwd entries: %w", err)
	}

	for _, line := range strings.Split(string(output), "\n") {
		parts := strings.Split(line, ":")
		if len(parts) >= 6 {
			uid := parts[2]
			var uidInt int
			fmt.Sscanf(uid, "%d", &uidInt)
			if uidInt >= 1000 && uidInt < 65534 {
				return parts[0], uid, parts[5], nil
			}
		}
	}

	return "", "", "", fmt.Errorf("no suitable user found")
}

// runUserSystemctl runs a systemctl --user command as the real user (not root)
func runUserSystemctl(args ...string) error {
	username, uid, _, err := getRealUser()
	if err != nil {
		return fmt.Errorf("failed to determine user: %w", err)
	}

	// Build the command with proper environment
	xdgRuntimeDir := fmt.Sprintf("/run/user/%s", uid)
	dbusAddr := fmt.Sprintf("unix:path=%s/bus", xdgRuntimeDir)

	// Use sudo -u to run as the user with proper environment
	cmdArgs := []string{
		"-u", username,
		fmt.Sprintf("XDG_RUNTIME_DIR=%s", xdgRuntimeDir),
		fmt.Sprintf("DBUS_SESSION_BUS_ADDRESS=%s", dbusAddr),
		"systemctl", "--user",
	}
	cmdArgs = append(cmdArgs, args...)

	cmd := exec.Command("sudo", cmdArgs...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w (output: %s)", err, strings.TrimSpace(string(output)))
	}
	return nil
}

// getUserConfigPath returns the user-specific config path for the real user (not root)
func getUserConfigPath() string {
	_, _, homeDir, err := getRealUser()
	if err != nil {
		// Fallback to os.UserHomeDir if we can't determine real user
		homeDir, err = os.UserHomeDir()
		if err != nil {
			log.Printf("Failed to get home directory: %v", err)
			return systemConfigPath
		}
	}
	return fmt.Sprintf("%s/.config/snapclient/options", homeDir)
}

// NewManager creates a new Snapclient manager
func NewManager(enabled bool) *Manager {
	return &Manager{
		enabled:        enabled,
		executablePath: defaultExecutablePath,
		configPath:     getUserConfigPath(),
	}
}

// IsEnabled returns whether Snapclient integration is enabled
func (m *Manager) IsEnabled() bool {
	return m.enabled
}

// GetVersion returns the Snapclient version
func (m *Manager) GetVersion() (string, error) {
	if !m.enabled {
		return "", fmt.Errorf("snapclient integration not enabled")
	}

	cmd := exec.Command(m.executablePath, "--version")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("failed to get version: %w", err)
	}

	// Parse version from output (format: "snapclient v0.x.x")
	versionRegex := regexp.MustCompile(`snapclient v(\d+\.\d+\.\d+)`)
	matches := versionRegex.FindStringSubmatch(string(output))
	if len(matches) >= 2 {
		return matches[1], nil
	}

	return strings.TrimSpace(string(output)), nil
}

// ListPCMDevices returns the list of available PCM devices (soundcards)
func (m *Manager) ListPCMDevices() ([]Player, error) {
	if !m.enabled {
		return nil, fmt.Errorf("snapclient integration not enabled")
	}

	cmd := exec.Command(m.executablePath, "-l")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("failed to list PCM devices: %w", err)
	}

	devices := []Player{}
	lines := strings.Split(string(output), "\n")

	// Parse the output - format is:
	// "0: null"
	// "Description line 1"
	// "Description line 2" (optional)
	// "" (blank line separator)
	// "1: pipewire"
	// ...
	var currentDevice *Player
	var descLines []string

	for _, line := range lines {
		line = strings.TrimSpace(line)

		// Blank line marks end of current device entry
		if line == "" {
			if currentDevice != nil && len(devices) > 0 {
				// Join all description lines collected for this device
				devices[len(devices)-1].Description = strings.Join(descLines, " - ")
				currentDevice = nil
				descLines = nil
			}
			continue
		}

		// Check if this is a device line (starts with digit(s) followed by ":")
		if len(line) > 0 && line[0] >= '0' && line[0] <= '9' {
			colonIdx := strings.Index(line, ":")
			if colonIdx > 0 {
				// This is a device name line like "0: null" or "3: hw:CARD=PCH,DEV=0"
				deviceName := strings.TrimSpace(line[colonIdx+1:])
				currentDevice = &Player{
					Name:        deviceName,
					Description: "",
					Available:   checkSoundcardExists(deviceName), // Check if device exists in aplay -l
				}
				devices = append(devices, *currentDevice)
				descLines = nil
				continue
			}
		}

		// This is a description line for the current device
		if currentDevice != nil {
			descLines = append(descLines, line)
		}
	}

	// Handle last device if file doesn't end with blank line
	if currentDevice != nil && len(devices) > 0 && len(descLines) > 0 {
		devices[len(devices)-1].Description = strings.Join(descLines, " - ")
	}

	// If no devices found, add a default option
	if len(devices) == 0 {
		devices = append(devices, Player{
			Name:        "default",
			Description: "Default PCM device",
			Available:   true, // default is always available
		})
	}

	return devices, nil
}

// GetConfig reads the current configuration from /etc/default/snapclient
func (m *Manager) GetConfig() (Config, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	config := Config{
		Player: defaultPlayer,
	}

	if !m.enabled {
		return config, fmt.Errorf("snapclient integration not enabled")
	}

	file, err := os.Open(m.configPath)
	if err != nil {
		if os.IsNotExist(err) {
			// Config file doesn't exist, return defaults
			return config, nil
		}
		return config, fmt.Errorf("failed to open config file: %w", err)
	}
	defer file.Close()

	// Parse SNAPCLIENT_OPTS line
	scanner := bufio.NewScanner(file)
	optsRegex := regexp.MustCompile(`SNAPCLIENT_OPTS="([^"]*)"`)

	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "#") {
			continue
		}

		matches := optsRegex.FindStringSubmatch(line)
		if len(matches) >= 2 {
			opts := matches[1]
			config = parseOptions(opts)
			break
		}
	}

	if err := scanner.Err(); err != nil {
		return config, fmt.Errorf("error reading config file: %w", err)
	}

	// Check if soundcard is available and get current ALSA volume if player is "alsa"
	// Note: SoundcardAvailable is only relevant for ALSA player, defaults to false for other players
	if config.Player == "alsa" {
		// Check if soundcard exists in the system
		config.SoundcardAvailable = checkSoundcardExists(config.Soundcard)
		
		// Skip volume retrieval for bluealsa - it doesn't support standard ALSA mixer controls
		// BlueALSA volume is controlled via Bluetooth protocol, not amixer
		if strings.Contains(strings.ToLower(config.Soundcard), "bluealsa") {
			config.Volume = 100 // BlueALSA doesn't use amixer volume
		} else if config.SoundcardAvailable {
			// Only attempt to get volume for non-bluealsa soundcards
			volume, err := m.GetAlsaVolume(config.Soundcard)
			if err != nil {
				// Only log once, not on every status check
				config.Volume = 100 // Default to 100% if we can't get current volume
			} else {
				config.Volume = volume
			}
		} else {
			config.Volume = 100 // Default to 100% when soundcard is not available
		}
	} else {
		// For non-ALSA players, volume control is not applicable
		// Set to 100 to avoid showing 0 in the UI
		config.Volume = 100
	}

	return config, nil
}

// parseOptions parses command-line options from SNAPCLIENT_OPTS
func parseOptions(opts string) Config {
	config := Config{
		Player: defaultPlayer,
	}

	// Split by spaces, but respect quoted values
	parts := strings.Fields(opts)

	for i := 0; i < len(parts); i++ {
		part := parts[i]

		// Handle --host or -h (deprecated but still support for backward compatibility)
		if (part == "--host" || part == "-h") && i+1 < len(parts) {
			config.Host = parts[i+1]
			i++
			continue
		} else if strings.HasPrefix(part, "--host=") {
			config.Host = strings.TrimPrefix(part, "--host=")
			continue
		}

		// Handle --hostID or -i
		if (part == "--hostID" || part == "-i") && i+1 < len(parts) {
			config.InstanceID = parts[i+1]
			i++
			continue
		} else if strings.HasPrefix(part, "--hostID=") {
			config.InstanceID = strings.TrimPrefix(part, "--hostID=")
			continue
		}

		// Handle --player (no short form)
		if part == "--player" && i+1 < len(parts) {
			config.Player = parts[i+1]
			i++
			continue
		} else if strings.HasPrefix(part, "--player=") {
			config.Player = strings.TrimPrefix(part, "--player=")
			continue
		}

		// Handle --soundcard or -s
		if (part == "--soundcard" || part == "-s") && i+1 < len(parts) {
			config.Soundcard = parts[i+1]
			i++
			continue
		} else if strings.HasPrefix(part, "--soundcard=") {
			config.Soundcard = strings.TrimPrefix(part, "--soundcard=")
			continue
		}

		// Handle positional argument (server URI) - anything that doesn't start with --
		// or looks like a URI with scheme
		if !strings.HasPrefix(part, "--") && !strings.HasPrefix(part, "-") {
			// This is likely the server URI (may include ws://, wss://, tcp:// scheme)
			if config.Host == "" || strings.Contains(part, "://") {
				config.Host = part
			}
		}
	}

	return config
}

// escapeShellArg escapes a string for safe use in shell scripts
func escapeShellArg(arg string) string {
	// Replace backslash with double backslash
	arg = strings.ReplaceAll(arg, `\`, `\\`)
	// Replace double quote with escaped double quote
	arg = strings.ReplaceAll(arg, `"`, `\"`)
	return arg
}

// ensureURIScheme ensures the host has a proper URI scheme (ws://)
func ensureURIScheme(host string) string {
	if host == "" {
		return host
	}
	// If it already has a scheme, return as is
	if strings.HasPrefix(host, "ws://") || strings.HasPrefix(host, "wss://") ||
		strings.HasPrefix(host, "tcp://") {
		return host
	}
	// Default to ws:// scheme
	return "ws://" + host
}

// SetConfig writes the configuration to /etc/default/snapclient
func (m *Manager) SetConfig(config Config) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.enabled {
		return fmt.Errorf("snapclient integration not enabled")
	}

	// Build the SNAPCLIENT_OPTS string with escaped values
	var opts []string

	if config.InstanceID != "" {
		opts = append(opts, fmt.Sprintf("--hostID %s", escapeShellArg(config.InstanceID)))
	}

	if config.Player != "" {
		opts = append(opts, fmt.Sprintf("--player %s", escapeShellArg(config.Player)))
	} else {
		opts = append(opts, fmt.Sprintf("--player %s", defaultPlayer))
	}

	if config.Soundcard != "" {
		opts = append(opts, fmt.Sprintf("--soundcard %s", escapeShellArg(config.Soundcard)))
	}

	// Add server URI as positional argument (not deprecated --host flag)
	// Ensure the URI has a proper scheme (ws://, wss://, or tcp://)
	if config.Host != "" {
		opts = append(opts, escapeShellArg(ensureURIScheme(config.Host)))
	}

	optsStr := strings.Join(opts, " ")

	// Create the config file content
	content := fmt.Sprintf(`# Snapclient configuration (auto-generated)
START_SNAPCLIENT=true
SNAPCLIENT_OPTS="%s"
`, optsStr)

	// Ensure the config directory exists
	configDir := filepath.Dir(m.configPath)
	if err := os.MkdirAll(configDir, 0755); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}

	// Write to temporary file first, then move
	tmpPath := m.configPath + ".tmp"
	if err := os.WriteFile(tmpPath, []byte(content), 0644); err != nil {
		return fmt.Errorf("failed to write config file: %w", err)
	}

	if err := os.Rename(tmpPath, m.configPath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("failed to save config file: %w", err)
	}

	log.Printf("Snapclient configuration saved to %s", m.configPath)
	
	return nil
}

// GetStatus returns the current status of the Snapclient service
func (m *Manager) GetStatus() (Status, error) {
	status := Status{
		Running: false,
	}

	if !m.enabled {
		return status, fmt.Errorf("snapclient integration not enabled")
	}

	// Check if it's a system service or user service
	status.IsSystemService = m.IsSystemService()
	status.UserServiceEnabled = m.IsUserServiceEnabled()

	// Check if service is running (as user service)
	// Need to run as the actual user, not root
	username, uid, _, err := getRealUser()
	if err == nil {
		xdgRuntimeDir := fmt.Sprintf("/run/user/%s", uid)
		dbusAddr := fmt.Sprintf("unix:path=%s/bus", xdgRuntimeDir)

		cmd := exec.Command("sudo", "-u", username,
			fmt.Sprintf("XDG_RUNTIME_DIR=%s", xdgRuntimeDir),
			fmt.Sprintf("DBUS_SESSION_BUS_ADDRESS=%s", dbusAddr),
			"systemctl", "--user", "is-active", "snapclient")
		output, cmdErr := cmd.CombinedOutput()
		activeStatus := strings.TrimSpace(string(output))
		if cmdErr == nil && activeStatus == "active" {
			status.Running = true
		}
		
		// Check if service is in failed state
		if activeStatus == "failed" {
			status.Failed = true
		}
	}

	// Get version
	version, err := m.GetVersion()
	if err != nil {
		log.Printf("Failed to get Snapclient version: %v", err)
	} else {
		status.Version = version
	}

	// Get configuration
	config, err := m.GetConfig()
	if err != nil {
		log.Printf("Failed to get Snapclient config: %v", err)
	} else {
		status.Config = config
	}

	return status, nil
}

// StartService starts the Snapclient systemd service
func (m *Manager) StartService() error {
	if !m.enabled {
		return fmt.Errorf("snapclient integration not enabled")
	}

	// First check if user service is enabled
	if !m.IsUserServiceEnabled() {
		return fmt.Errorf("user service not enabled. Enable it first via the UI")
	}

	log.Println("Starting Snapclient service...")
	if err := runUserSystemctl("start", "snapclient"); err != nil {
		return fmt.Errorf("failed to start service: %w", err)
	}

	log.Println("Snapclient service started successfully")
	return nil
}

// StopService stops the Snapclient systemd service
func (m *Manager) StopService() error {
	if !m.enabled {
		return fmt.Errorf("snapclient integration not enabled")
	}

	log.Println("Stopping Snapclient service...")
	if err := runUserSystemctl("stop", "snapclient"); err != nil {
		return fmt.Errorf("failed to stop service: %w", err)
	}

	log.Println("Snapclient service stopped successfully")
	return nil
}

// RestartService restarts the Snapclient systemd service
func (m *Manager) RestartService() error {
	if !m.enabled {
		return fmt.Errorf("snapclient integration not enabled")
	}

	// First check if user service is enabled
	if !m.IsUserServiceEnabled() {
		return fmt.Errorf("user service not enabled. Enable it first via the UI")
	}

	log.Println("Restarting Snapclient service...")
	if err := runUserSystemctl("restart", "snapclient"); err != nil {
		return fmt.Errorf("failed to restart service: %w", err)
	}

	log.Println("Snapclient service restarted successfully")
	return nil
}

// IsSystemService checks if Snapclient is running as a system service (root) instead of user service
// Returns true ONLY if system service is actively running or enabled (not just because user service isn't configured)
func (m *Manager) IsSystemService() bool {
	if !m.enabled {
		return false
	}

	// Check if system service is active
	cmd := exec.Command("systemctl", "is-active", "snapclient")
	output, err := cmd.CombinedOutput()
	if err == nil && strings.TrimSpace(string(output)) == "active" {
		return true
	}

	// Check if system service is enabled (even if not running)
	cmd = exec.Command("systemctl", "is-enabled", "snapclient")
	output, err = cmd.CombinedOutput()
	if err == nil {
		status := strings.TrimSpace(string(output))
		if status == "enabled" || status == "static" || status == "alias" {
			return true
		}
	}

	// System service is not active/enabled
	return false
}

// IsUserServiceEnabled checks if the user service is enabled
func (m *Manager) IsUserServiceEnabled() bool {
	if !m.enabled {
		return false
	}

	// Try using the helper for proper user context
	username, uid, _, err := getRealUser()
	if err != nil {
		return false
	}

	xdgRuntimeDir := fmt.Sprintf("/run/user/%s", uid)
	dbusAddr := fmt.Sprintf("unix:path=%s/bus", xdgRuntimeDir)

	cmd := exec.Command("sudo", "-u", username,
		fmt.Sprintf("XDG_RUNTIME_DIR=%s", xdgRuntimeDir),
		fmt.Sprintf("DBUS_SESSION_BUS_ADDRESS=%s", dbusAddr),
		"systemctl", "--user", "is-enabled", "snapclient")
	output, err := cmd.CombinedOutput()
	if err == nil {
		status := strings.TrimSpace(string(output))
		if status == "enabled" || status == "static" || status == "alias" {
			return true
		}
	}
	return false
}

// EnableResult contains the result of enabling the user service
type EnableResult struct {
	Success bool   `json:"success"`
	Error   string `json:"error,omitempty"`
}

// EnableUserService enables and starts the user service without migrating from system service
func (m *Manager) EnableUserService() EnableResult {
	result := EnableResult{Success: false}

	if !m.enabled {
		result.Error = "Snapclient integration not enabled"
		return result
	}

	_, _, homeDir, err := getRealUser()
	if err != nil {
		result.Error = fmt.Sprintf("Failed to get real user: %v", err)
		return result
	}

	// Create directories if they don't exist
	systemdUserDir := fmt.Sprintf("%s/.config/systemd/user", homeDir)
	snapclientConfigDir := fmt.Sprintf("%s/.config/snapclient", homeDir)

	if err := os.MkdirAll(systemdUserDir, 0755); err != nil {
		result.Error = fmt.Sprintf("Failed to create systemd user directory: %v", err)
		return result
	}

	if err := os.MkdirAll(snapclientConfigDir, 0755); err != nil {
		result.Error = fmt.Sprintf("Failed to create snapclient config directory: %v", err)
		return result
	}

	// Check if service file exists, create if not
	serviceFile := fmt.Sprintf("%s/snapclient.service", systemdUserDir)
	if _, err := os.Stat(serviceFile); os.IsNotExist(err) {
		serviceContent := `[Unit]
Description=Snapcast client (user)
Documentation=man:snapclient(1)
Wants=network-online.target
After=network-online.target sound.target

[Service]
EnvironmentFile=-%h/.config/snapclient/options
ExecStart=/usr/bin/snapclient --logsink=system $SNAPCLIENT_OPTS
Restart=on-failure

[Install]
WantedBy=default.target
`
		if err := os.WriteFile(serviceFile, []byte(serviceContent), 0644); err != nil {
			result.Error = fmt.Sprintf("Failed to create service file: %v", err)
			return result
		}
	}

	// Reload user daemon
	if err := runUserSystemctl("daemon-reload"); err != nil {
		log.Printf("Warning: daemon-reload failed: %v", err)
	}

	// Enable user service
	if err := runUserSystemctl("enable", "snapclient"); err != nil {
		result.Error = fmt.Sprintf("Failed to enable user service: %v", err)
		return result
	}

	// Start user service
	if err := runUserSystemctl("start", "snapclient"); err != nil {
		result.Error = fmt.Sprintf("Failed to start user service: %v", err)
		return result
	}

	result.Success = true
	log.Println("Successfully enabled and started Snapclient user service")
	return result
}

// extractCardName extracts the card name from various soundcard formats
// Examples:
//   - "front:CARD=Audio,DEV=0" -> "Audio"
//   - "hw:CARD=PCH,DEV=0" -> "PCH"
//   - "hw:0" -> "0"
//   - "default" -> ""
//   - "" -> ""
func extractCardName(soundcard string) string {
	if soundcard == "" || soundcard == "default" {
		return ""
	}

	// Extract CARD name from formats like "front:CARD=Audio,DEV=0" or "hw:CARD=PCH,DEV=0"
	if strings.Contains(soundcard, "CARD=") {
		cardStart := strings.Index(soundcard, "CARD=")
		if cardStart >= 0 {
			cardStart += len("CARD=")
			cardEnd := strings.Index(soundcard[cardStart:], ",")
			if cardEnd < 0 {
				cardEnd = len(soundcard[cardStart:])
			}
			return soundcard[cardStart : cardStart+cardEnd]
		}
	}

	// Handle "hw:0" or "hw:1" format - extract the number
	if strings.HasPrefix(soundcard, "hw:") && !strings.Contains(soundcard, "CARD=") {
		return strings.TrimPrefix(soundcard, "hw:")
	}

	// For other formats like "bluealsa", return as-is
	return soundcard
}

// checkSoundcardExists checks if a soundcard exists in the system using aplay -l
// Returns true if the soundcard is found or if soundcard is empty/default
func checkSoundcardExists(soundcard string) bool {
	// Empty or default soundcard is always valid
	if soundcard == "" || soundcard == "default" {
		return true
	}

	// Special case for bluealsa - it's a virtual ALSA plugin, not a hardware device
	// It doesn't appear in aplay -l and we can't control its volume with amixer
	// So we should mark it as unavailable for volume control
	if soundcard == "bluealsa" {
		return false
	}

	// Extract the card name to check
	cardName := extractCardName(soundcard)
	if cardName == "" {
		// If we can't extract a card name, check against aplay -l anyway
		// This handles special cases or unknown formats
		cardName = soundcard
	}

	// Run aplay -l to list hardware devices
	cmd := exec.Command("aplay", "-l")
	output, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("Warning: Failed to run aplay -l: %v", err)
		// If aplay fails, we can't verify, so return false for safety
		// This prevents errors from amixer trying to access non-existent devices
		return false
	}

	outputStr := string(output)

	// Check if the card name appears in the output
	// aplay -l output format:
	// card 0: Audio [AB13X USB Audio], device 0: USB Audio [USB Audio]
	// We need to check for either:
	// - "card X: CardName" where X matches the card number
	// - or the card name appears in the output

	lines := strings.Split(outputStr, "\n")
	for _, line := range lines {
		// Check if this line starts with "card " (hardware device line)
		if strings.HasPrefix(line, "card ") {
			// Extract card number and name from line like "card 0: Audio [AB13X USB Audio]"
			colonIdx := strings.Index(line, ":")
			if colonIdx > 0 {
				// Get the part after "card " and before ":"
				cardNumStr := strings.TrimSpace(line[len("card "):colonIdx])

				// Get the part after ":" which contains the card name
				afterColon := line[colonIdx+1:]

				// Check if our card name matches either the number or the name
				if cardNumStr == cardName {
					return true
				}

				// Check if the card name appears in the line (case-insensitive)
				if strings.Contains(strings.ToLower(afterColon), strings.ToLower(cardName)) {
					return true
				}
			}
		}
	}

	return false
}

// convertToAmixerDevice converts a soundcard device name to amixer-compatible format
// Examples:
//   - "front:CARD=Audio,DEV=0" -> "hw:Audio"
//   - "hw:CARD=PCH,DEV=0" -> "hw:PCH"
//   - "hw:0" -> "hw:0"
//   - "default" -> "default"
//   - "" -> ""
func convertToAmixerDevice(soundcard string) string {
	if soundcard == "" || soundcard == "default" {
		return soundcard
	}

	// If it already starts with "hw:" and doesn't contain "CARD=", return as-is
	if strings.HasPrefix(soundcard, "hw:") && !strings.Contains(soundcard, "CARD=") {
		return soundcard
	}

	// Extract CARD name from formats like "front:CARD=Audio,DEV=0" or "hw:CARD=PCH,DEV=0"
	if strings.Contains(soundcard, "CARD=") {
		// Find the CARD= part
		cardStart := strings.Index(soundcard, "CARD=")
		if cardStart >= 0 {
			cardStart += len("CARD=")
			// Find the end of the card name (comma or end of string)
			cardEnd := strings.Index(soundcard[cardStart:], ",")
			if cardEnd < 0 {
				cardEnd = len(soundcard[cardStart:])
			}
			cardName := soundcard[cardStart : cardStart+cardEnd]
			return fmt.Sprintf("hw:%s", cardName)
		}
	}

	// If it's something else like "bluealsa", return as-is
	return soundcard
}

// SetAlsaVolume sets the ALSA volume using amixer command
// soundcard can be empty (uses default), or a specific device like "hw:1"
// volume is a percentage from 0 to 100
func (m *Manager) SetAlsaVolume(soundcard string, volume int) error {
	if !m.enabled {
		return fmt.Errorf("snapclient integration not enabled")
	}

	// BlueALSA doesn't support standard amixer volume control
	// Volume is controlled via Bluetooth A2DP protocol
	if strings.Contains(strings.ToLower(soundcard), "bluealsa") {
		return fmt.Errorf("volume control not supported for BlueALSA devices - use device volume controls instead")
	}

	// Validate volume range
	if volume < 0 || volume > 100 {
		return fmt.Errorf("volume must be between 0 and 100, got %d", volume)
	}

	// Check if soundcard exists in the system
	if !checkSoundcardExists(soundcard) {
		return fmt.Errorf("soundcard '%s' not found in system (check 'aplay -l' output)", soundcard)
	}

	// Convert soundcard to amixer-compatible device format
	device := convertToAmixerDevice(soundcard)

	// Build the amixer command
	// Format: amixer [-D device] set PCM volume%
	args := []string{}
	
	// Add device specification if provided
	if device != "" {
		args = append(args, "-D", device)
	}
	
	args = append(args, "set", "PCM", fmt.Sprintf("%d%%", volume))

	cmd := exec.Command("amixer", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to set volume with amixer: %w (output: %s)", err, string(output))
	}

	log.Printf("ALSA volume set to %d%% (device: %s -> %s)", volume, soundcard, device)
	return nil
}

// GetAlsaVolume gets the current ALSA volume using amixer command
// Returns volume percentage (0-100) or error
func (m *Manager) GetAlsaVolume(soundcard string) (int, error) {
	if !m.enabled {
		return 0, fmt.Errorf("snapclient integration not enabled")
	}

	// Check if soundcard exists in the system
	if !checkSoundcardExists(soundcard) {
		return 0, fmt.Errorf("soundcard '%s' not found in system (check 'aplay -l' output)", soundcard)
	}

	// Convert soundcard to amixer-compatible device format
	device := convertToAmixerDevice(soundcard)

	// Build the amixer command
	// Format: amixer [-D device] get PCM
	args := []string{}
	
	// Add device specification if provided
	if device != "" {
		args = append(args, "-D", device)
	}
	
	args = append(args, "get", "PCM")

	cmd := exec.Command("amixer", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("failed to get volume with amixer: %w (output: %s)", err, string(output))
	}

	// Parse the output to extract volume percentage
	// Output format example: "Simple mixer control 'PCM',0\n  Capabilities: pvolume pvolume-joined pswitch pswitch-joined\n  Playback channels: Mono\n  Limits: Playback 0 - 255\n  Mono: Playback 255 [100%] [0.00dB] [on]"
	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		// Look for line with percentage like "[50%]"
		if strings.Contains(line, "[") && strings.Contains(line, "%]") {
			// Extract percentage value
			startIdx := strings.Index(line, "[")
			endIdx := strings.Index(line, "%]")
			if startIdx >= 0 && endIdx > startIdx {
				volumeStr := strings.TrimSpace(line[startIdx+1 : endIdx])
				volume := volumeRegex.FindString(volumeStr)
				if volume != "" {
					var vol int
					fmt.Sscanf(volume, "%d", &vol)
					return vol, nil
				}
			}
		}
	}

	return 0, fmt.Errorf("could not parse volume from amixer output")
}

// MigrateToUserService attempts to migrate from system service to user service
func (m *Manager) MigrateToUserService() MigrationResult {
	result := MigrationResult{Success: false}

	if !m.enabled {
		result.Error = "Snapclient integration not enabled"
		return result
	}

	_, _, homeDir, err := getRealUser()
	if err != nil {
		result.Error = fmt.Sprintf("Failed to get real user: %v", err)
		return result
	}

	// Create directories
	systemdUserDir := fmt.Sprintf("%s/.config/systemd/user", homeDir)
	snapclientConfigDir := fmt.Sprintf("%s/.config/snapclient", homeDir)

	if err := os.MkdirAll(systemdUserDir, 0755); err != nil {
		result.Error = fmt.Sprintf("Failed to create systemd user directory: %v", err)
		return result
	}

	if err := os.MkdirAll(snapclientConfigDir, 0755); err != nil {
		result.Error = fmt.Sprintf("Failed to create snapclient config directory: %v", err)
		return result
	}

	// Create user service file
	serviceFile := fmt.Sprintf("%s/snapclient.service", systemdUserDir)
	serviceContent := `[Unit]
Description=Snapcast client (user)
Documentation=man:snapclient(1)
Wants=network-online.target
After=network-online.target sound.target

[Service]
EnvironmentFile=-%h/.config/snapclient/options
ExecStart=/usr/bin/snapclient --logsink=system $SNAPCLIENT_OPTS
Restart=on-failure

[Install]
WantedBy=default.target
`

	if err := os.WriteFile(serviceFile, []byte(serviceContent), 0644); err != nil {
		result.Error = fmt.Sprintf("Failed to create service file: %v", err)
		return result
	}

	// Get current config or use defaults
	var currentConfig Config
	if _, err := os.Stat(systemConfigPath); err == nil {
		// Try to read system config
		m.configPath = systemConfigPath
		currentConfig, _ = m.GetConfig()
	}
	// Set default if no host specified
	if currentConfig.Host == "" {
		currentConfig.Host = "ws://127.0.0.1"
	}
	if currentConfig.Player == "" {
		currentConfig.Player = defaultPlayer
	}

	// Create user config file with current or default settings
	m.configPath = getUserConfigPath()
	if err := m.SetConfig(currentConfig); err != nil {
		result.Error = fmt.Sprintf("Failed to create user config file: %v", err)
		return result
	}

	// Try to stop and disable system service
	manualSteps := []string{}

	// Stop system service
	cmd := exec.Command("sudo", "systemctl", "stop", "snapclient")
	if err := cmd.Run(); err != nil {
		manualSteps = append(manualSteps, "sudo systemctl stop snapclient")
	}

	// Disable system service
	cmd = exec.Command("sudo", "systemctl", "disable", "snapclient")
	if err := cmd.Run(); err != nil {
		manualSteps = append(manualSteps, "sudo systemctl disable snapclient")
	}

	// Mask system service
	cmd = exec.Command("sudo", "systemctl", "mask", "snapclient")
	if err := cmd.Run(); err != nil {
		manualSteps = append(manualSteps, "sudo systemctl mask snapclient")
	}

	// Reload user daemon
	if err := runUserSystemctl("daemon-reload"); err != nil {
		manualSteps = append(manualSteps, "systemctl --user daemon-reload")
	}

	// Enable and start user service
	if err := runUserSystemctl("enable", "snapclient"); err != nil {
		manualSteps = append(manualSteps, "systemctl --user enable snapclient")
	}

	if err := runUserSystemctl("start", "snapclient"); err != nil {
		manualSteps = append(manualSteps, "systemctl --user start snapclient")
	}

	// Check if we need manual intervention
	if len(manualSteps) > 0 {
		result.Success = false
		result.ManualSteps = manualSteps
		result.Error = "Some steps require manual intervention. Please run the following commands:"
	} else {
		result.Success = true
		log.Println("Successfully migrated Snapclient to user service")
	}

	return result
}

// StreamLogs streams the systemd journal logs for the snapclient service
// The function returns a channel that will receive log lines and an error if the stream cannot be started.
// The caller should read from the returned channel until it is closed.
// Call the returned stop function to stop the log stream.
func (m *Manager) StreamLogs(ctx context.Context, lines int) (<-chan string, func(), error) {
	if !m.enabled {
		return nil, nil, fmt.Errorf("snapclient integration not enabled")
	}

	// Create channel for log lines
	logChan := make(chan string, logChannelBufferSize)

	// Build journalctl command
	// --user-unit snapclient: user service unit name
	// -f: follow (stream new logs)
	// -n lines: show last N lines
	// -o cat: output format (just the message, no metadata)
	args := []string{"--user-unit", "snapclient", "-f", "-n", fmt.Sprintf("%d", lines), "-o", "cat"}
	cmd := exec.CommandContext(ctx, "journalctl", args...)

	// Get stdout pipe
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create stdout pipe: %w", err)
	}

	// Start the command
	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("failed to start journalctl: %w", err)
	}

	// Start goroutine to read logs
	go func() {
		defer close(logChan)
		defer cmd.Wait()

		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			select {
			case <-ctx.Done():
				return
			case logChan <- scanner.Text():
			}
		}

		if err := scanner.Err(); err != nil {
			log.Printf("Error reading logs: %v", err)
		}
	}()

	// Return stop function
	stop := func() {
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
	}

	return logChan, stop, nil
}
