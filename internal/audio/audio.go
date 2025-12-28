package audio

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// macAddressPattern validates MAC address format (XX:XX:XX:XX:XX:XX)
var macAddressPattern = regexp.MustCompile(`^([0-9A-Fa-f]{2}:){5}[0-9A-Fa-f]{2}$`)

// SetDefaultSink sets the Bluetooth device as the default audio output
// It uses ALSA with bluez-alsa (bluealsa) to configure the audio routing
func SetDefaultSink(address string) error {
	// Validate MAC address format to prevent command injection
	if !macAddressPattern.MatchString(address) {
		return fmt.Errorf("invalid MAC address format: %s", address)
	}

	// Create ALSA configuration for bluealsa device
	// bluealsa uses format: bluealsa:DEV=XX:XX:XX:XX:XX:XX,PROFILE=a2dp
	asoundConfig := fmt.Sprintf(`# Bluetooth audio device configuration (auto-generated)
pcm.!default {
    type plug
    slave.pcm {
        type bluealsa
        device "%s"
        profile "a2dp"
    }
}

ctl.!default {
    type bluealsa
}
`, address)

	// Write to user's .asoundrc file
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get home directory: %w", err)
	}

	asoundrcPath := filepath.Join(homeDir, ".asoundrc")
	if err := os.WriteFile(asoundrcPath, []byte(asoundConfig), 0644); err != nil {
		return fmt.Errorf("failed to write ALSA configuration: %w", err)
	}

	log.Printf("ALSA configuration written to %s for Bluetooth device %s", asoundrcPath, address)
	return nil
}

// IsAudioDevice checks if a device supports audio profiles based on its icon type
func IsAudioDevice(icon string) bool {
	audioIcons := []string{
		"audio-card",
		"audio-headphones",
		"audio-headset",
		"audio-speakers",
		"multimedia-player",
		"phone",
	}
	for _, audioIcon := range audioIcons {
		if icon == audioIcon {
			return true
		}
	}
	return false
}

// Manager handles ALSA audio routing configuration
type Manager struct {
	mu sync.RWMutex
}

// NewManager creates a new audio manager
func NewManager() *Manager {
	return &Manager{}
}

// GetCurrentDevice returns the MAC address of the current default Bluetooth device, if any
func (m *Manager) GetCurrentDevice() (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}

	asoundrcPath := filepath.Join(homeDir, ".asoundrc")
	file, err := os.Open(asoundrcPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil // No config file, no default device
		}
		return "", fmt.Errorf("failed to open ALSA config: %w", err)
	}
	defer file.Close()

	// Parse the .asoundrc file to find the device MAC address
	scanner := bufio.NewScanner(file)
	deviceRegex := regexp.MustCompile(`device\s+"([0-9A-Fa-f:]+)"`)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if matches := deviceRegex.FindStringSubmatch(line); len(matches) >= 2 {
			return matches[1], nil
		}
	}

	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("error reading ALSA config: %w", err)
	}

	return "", nil // No device found in config
}

// SetDefaultDevice sets a Bluetooth device as the default audio output
func (m *Manager) SetDefaultDevice(address string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := SetDefaultSink(address); err != nil {
		return err
	}

	log.Printf("Set Bluetooth device %s as default ALSA output", address)
	return nil
}
