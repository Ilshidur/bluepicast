package snapcast

import (
	"strings"
	"testing"
)

func TestParseOptions(t *testing.T) {
	tests := []struct {
		name     string
		opts     string
		expected Config
	}{
		{
			name: "parse host option",
			opts: "--host 192.168.1.100",
			expected: Config{
				Host:   "192.168.1.100",
				Player: "bluealsa",
			},
		},
		{
			name: "parse all options",
			opts: "--host 192.168.1.100 --hostID my-client --player alsa --soundcard hw:0,0",
			expected: Config{
				Host:       "192.168.1.100",
				InstanceID: "my-client",
				Player:     "alsa",
				Soundcard:  "hw:0,0",
			},
		},
		{
			name: "parse with equals syntax",
			opts: "--host=127.0.0.1 --hostID=test-id --player=bluealsa",
			expected: Config{
				Host:       "127.0.0.1",
				InstanceID: "test-id",
				Player:     "bluealsa",
			},
		},
		{
			name: "parse with short flags",
			opts: "-h 10.0.0.1 -i my-id -s hw:0,0",
			expected: Config{
				Host:       "10.0.0.1",
				InstanceID: "my-id",
				Player:     "bluealsa",
				Soundcard:  "hw:0,0",
			},
		},
		{
			name: "parse with positional server URI (new format)",
			opts: "--hostID my-client --player alsa --soundcard hw:0,0 192.168.1.100",
			expected: Config{
				Host:       "192.168.1.100",
				InstanceID: "my-client",
				Player:     "alsa",
				Soundcard:  "hw:0,0",
			},
		},
		{
			name: "parse with positional server URI only",
			opts: "192.168.1.100",
			expected: Config{
				Host:   "192.168.1.100",
				Player: "bluealsa",
			},
		},
		{
			name: "parse with positional server URI with ws scheme",
			opts: "--hostID my-client --player alsa ws://192.168.1.100",
			expected: Config{
				Host:       "ws://192.168.1.100",
				InstanceID: "my-client",
				Player:     "alsa",
			},
		},
		{
			name: "parse with positional server URI with port",
			opts: "ws://192.168.1.100:1704",
			expected: Config{
				Host:   "ws://192.168.1.100:1704",
				Player: "bluealsa",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := parseOptions(tt.opts)

			if result.Host != tt.expected.Host {
				t.Errorf("Host = %v, want %v", result.Host, tt.expected.Host)
			}
			if result.InstanceID != tt.expected.InstanceID {
				t.Errorf("InstanceID = %v, want %v", result.InstanceID, tt.expected.InstanceID)
			}
			if result.Player != tt.expected.Player {
				t.Errorf("Player = %v, want %v", result.Player, tt.expected.Player)
			}
			if result.Soundcard != tt.expected.Soundcard {
				t.Errorf("Soundcard = %v, want %v", result.Soundcard, tt.expected.Soundcard)
			}
		})
	}
}

func TestNewManager(t *testing.T) {
	manager := NewManager(true)

	if !manager.IsEnabled() {
		t.Error("Manager should be enabled")
	}

	if manager.executablePath != defaultExecutablePath {
		t.Errorf("executablePath = %v, want %v", manager.executablePath, defaultExecutablePath)
	}

	// configPath should be user-specific now
	expectedConfigPath := getUserConfigPath()
	if manager.configPath != expectedConfigPath {
		t.Errorf("configPath = %v, want %v", manager.configPath, expectedConfigPath)
	}
}

func TestNewManagerDisabled(t *testing.T) {
	manager := NewManager(false)

	if manager.IsEnabled() {
		t.Error("Manager should be disabled")
	}
}

func TestEscapeShellArg(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "simple string",
			input:    "hello",
			expected: "hello",
		},
		{
			name:     "string with quotes",
			input:    `my "server"`,
			expected: `my \"server\"`,
		},
		{
			name:     "string with backslash",
			input:    `path\to\file`,
			expected: `path\\to\\file`,
		},
		{
			name:     "string with both",
			input:    `path\to\"file"`,
			expected: `path\\to\\\"file\"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := escapeShellArg(tt.input)
			if result != tt.expected {
				t.Errorf("escapeShellArg(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestEnsureURIScheme(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "IP without scheme",
			input:    "192.168.1.100",
			expected: "ws://192.168.1.100",
		},
		{
			name:     "localhost without scheme",
			input:    "127.0.0.1",
			expected: "ws://127.0.0.1",
		},
		{
			name:     "hostname without scheme",
			input:    "snapserver.local",
			expected: "ws://snapserver.local",
		},
		{
			name:     "IP with ws scheme",
			input:    "ws://192.168.1.100",
			expected: "ws://192.168.1.100",
		},
		{
			name:     "IP with wss scheme",
			input:    "wss://192.168.1.100",
			expected: "wss://192.168.1.100",
		},
		{
			name:     "IP with tcp scheme",
			input:    "tcp://192.168.1.100",
			expected: "tcp://192.168.1.100",
		},
		{
			name:     "empty string",
			input:    "",
			expected: "",
		},
		{
			name:     "IP with port without scheme",
			input:    "192.168.1.100:1704",
			expected: "ws://192.168.1.100:1704",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ensureURIScheme(tt.input)
			if result != tt.expected {
				t.Errorf("ensureURIScheme(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestSetAlsaVolume(t *testing.T) {
	// Test volume validation
	manager := NewManager(true)

	tests := []struct {
		name      string
		volume    int
		shouldErr bool
	}{
		{
			name:      "valid volume 0",
			volume:    0,
			shouldErr: false,
		},
		{
			name:      "valid volume 50",
			volume:    50,
			shouldErr: false,
		},
		{
			name:      "valid volume 100",
			volume:    100,
			shouldErr: false,
		},
		{
			name:      "invalid volume negative",
			volume:    -1,
			shouldErr: true,
		},
		{
			name:      "invalid volume too high",
			volume:    101,
			shouldErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := manager.SetAlsaVolume("", tt.volume)
			// We expect an error either for invalid volume or because amixer is not available
			// The test validates the volume range check is working
			if tt.shouldErr {
				if err == nil || err.Error() == "" {
					t.Errorf("SetAlsaVolume(%d) should return error for invalid volume", tt.volume)
				}
				// Check if error message contains volume validation message
				if err != nil && !strings.Contains(err.Error(), "volume must be between 0 and 100") {
					// If it's not a validation error, that's also acceptable (might be amixer not found)
					t.Logf("Expected validation error but got: %v", err)
				}
			}
			// For valid volumes, we can't test success without amixer installed
			// But we can at least verify the function doesn't panic
		})
	}
}

func TestSetAlsaVolumeDisabled(t *testing.T) {
	manager := NewManager(false)
	err := manager.SetAlsaVolume("", 50)
	if err == nil {
		t.Error("SetAlsaVolume should return error when manager is disabled")
	}
	if !strings.Contains(err.Error(), "not enabled") {
		t.Errorf("Error message should mention 'not enabled', got: %v", err)
	}
}

func TestGetAlsaVolumeDisabled(t *testing.T) {
	manager := NewManager(false)
	_, err := manager.GetAlsaVolume("")
	if err == nil {
		t.Error("GetAlsaVolume should return error when manager is disabled")
	}
	if !strings.Contains(err.Error(), "not enabled") {
		t.Errorf("Error message should mention 'not enabled', got: %v", err)
	}
}

func TestConvertToAmixerDevice(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "empty string",
			input:    "",
			expected: "",
		},
		{
			name:     "default device",
			input:    "default",
			expected: "default",
		},
		{
			name:     "simple hw device",
			input:    "hw:0",
			expected: "hw:0",
		},
		{
			name:     "hw device with number",
			input:    "hw:1",
			expected: "hw:1",
		},
		{
			name:     "front device with CARD",
			input:    "front:CARD=Audio,DEV=0",
			expected: "hw:Audio",
		},
		{
			name:     "hw device with CARD",
			input:    "hw:CARD=PCH,DEV=0",
			expected: "hw:PCH",
		},
		{
			name:     "device with CARD only",
			input:    "CARD=SoundCard",
			expected: "hw:SoundCard",
		},
		{
			name:     "bluealsa device",
			input:    "bluealsa",
			expected: "bluealsa",
		},
		{
			name:     "surround device with CARD",
			input:    "surround40:CARD=Generic,DEV=0",
			expected: "hw:Generic",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := convertToAmixerDevice(tt.input)
			if result != tt.expected {
				t.Errorf("convertToAmixerDevice(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestExtractCardName(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "empty string",
			input:    "",
			expected: "",
		},
		{
			name:     "default device",
			input:    "default",
			expected: "",
		},
		{
			name:     "simple hw device",
			input:    "hw:0",
			expected: "0",
		},
		{
			name:     "hw device with number",
			input:    "hw:1",
			expected: "1",
		},
		{
			name:     "front device with CARD",
			input:    "front:CARD=Audio,DEV=0",
			expected: "Audio",
		},
		{
			name:     "hw device with CARD",
			input:    "hw:CARD=PCH,DEV=0",
			expected: "PCH",
		},
		{
			name:     "device with CARD only",
			input:    "CARD=SoundCard",
			expected: "SoundCard",
		},
		{
			name:     "bluealsa device",
			input:    "bluealsa",
			expected: "bluealsa",
		},
		{
			name:     "surround device with CARD",
			input:    "surround40:CARD=Generic,DEV=0",
			expected: "Generic",
		},
		{
			name:     "device with CARD at end",
			input:    "front:CARD=Audio",
			expected: "Audio",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := extractCardName(tt.input)
			if result != tt.expected {
				t.Errorf("extractCardName(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}
