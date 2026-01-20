package web

import (
	"context"
	"crypto/tls"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/Ilshidur/bluepicast/internal/audio"
	"github.com/Ilshidur/bluepicast/internal/bluetooth"
	"github.com/Ilshidur/bluepicast/internal/snapcast"
)

//go:embed static/*
var staticFiles embed.FS

// Message types for WebSocket communication
type MessageType string

const (
	MsgTypeDevices                   MessageType = "devices"
	MsgTypeScan                      MessageType = "scan"
	MsgTypeStopScan                  MessageType = "stop_scan"
	MsgTypePair                      MessageType = "pair"
	MsgTypeConnect                   MessageType = "connect"
	MsgTypeDisconnect                MessageType = "disconnect"
	MsgTypeRemove                    MessageType = "remove"
	MsgTypePairAndConnect            MessageType = "pair_and_connect"
	MsgTypeError                     MessageType = "error"
	MsgTypeStatus                    MessageType = "status"
	MsgTypeAlsaConfig                MessageType = "alsa_config"
	MsgTypeAlsaGetConfig             MessageType = "alsa_get_config"
	MsgTypeAlsaSetConfig             MessageType = "alsa_set_config"
	MsgTypeAlsaSetDevice             MessageType = "alsa_set_device"
	MsgTypeSnapclientStatus          MessageType = "snapclient_status"
	MsgTypeSnapclientGetStatus       MessageType = "snapclient_get_status"
	MsgTypeSnapclientStart           MessageType = "snapclient_start"
	MsgTypeSnapclientStop            MessageType = "snapclient_stop"
	MsgTypeSnapclientRestart         MessageType = "snapclient_restart"
	MsgTypeSnapclientSetConfig       MessageType = "snapclient_set_config"
	MsgTypeSnapclientPlayers         MessageType = "snapclient_players"
	MsgTypeSnapclientGetPlayers      MessageType = "snapclient_get_players"
	MsgTypeSnapclientPCMDevices      MessageType = "snapclient_pcm_devices"
	MsgTypeSnapclientGetPCM          MessageType = "snapclient_get_pcm"
	MsgTypeSnapclientMigrate           MessageType = "snapclient_migrate"
	MsgTypeSnapclientMigrationResult   MessageType = "snapclient_migration_result"
	MsgTypeSnapclientEnableUserService MessageType = "snapclient_enable_user_service"
	MsgTypeSnapclientEnableResult      MessageType = "snapclient_enable_result"
	MsgTypeSnapclientSetVolume         MessageType = "snapclient_set_volume"
	MsgTypeSnapclientGetVolume       MessageType = "snapclient_get_volume"
	MsgTypeSnapclientStartLogs       MessageType = "snapclient_start_logs"
	MsgTypeSnapclientStopLogs        MessageType = "snapclient_stop_logs"
	MsgTypeSnapclientLog             MessageType = "snapclient_log"
)

// Message represents a WebSocket message
type Message struct {
	Type    MessageType     `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// DevicesPayload contains the list of discovered devices
type DevicesPayload struct {
	Devices  []*bluetooth.Device `json:"devices"`
	Scanning bool                `json:"scanning"`
}

// StatusPayload contains status information
type StatusPayload struct {
	Scanning bool   `json:"scanning"`
	Message  string `json:"message,omitempty"`
}

// DeviceActionPayload contains a device address for actions
type DeviceActionPayload struct {
	Address string `json:"address"`
}

// ErrorPayload contains error information
type ErrorPayload struct {
	Message string `json:"message"`
}

// AlsaConfig represents the ALSA routing configuration
type AlsaConfig struct {
	AutoRoute     bool   `json:"autoRoute"`
	CurrentDevice string `json:"currentDevice"`
}

// VolumePayload contains volume information
type VolumePayload struct {
	Volume int `json:"volume"`
}

// SoundcardPayload contains soundcard information for volume queries
type SoundcardPayload struct {
	Soundcard string `json:"soundcard"`
}

// LogPayload contains a single log line
type LogPayload struct {
	Line string `json:"line"`
}

// client wraps a websocket connection with a mutex for safe concurrent writes
type client struct {
	conn           *websocket.Conn
	mu             sync.Mutex
	logStopFunc    func()          // Function to stop log streaming
	logStopFuncMu  sync.Mutex      // Mutex for log stop function
}

// Server handles HTTP and WebSocket connections
type Server struct {
	adapter         *bluetooth.Adapter
	audioMgr        *audio.Manager
	snapclientMgr   *snapcast.Manager
	upgrader        websocket.Upgrader
	clients         map[*client]bool
	clientsMu       sync.RWMutex
	port            int
	tlsConfig       *tls.Config
	alsaAutoRoute   bool
	alsaAutoRouteMu sync.RWMutex
}

// NewServer creates a new web server
func NewServer(adapter *bluetooth.Adapter, audioMgr *audio.Manager, snapclientMgr *snapcast.Manager, port int, tlsConfig *tls.Config) *Server {
	s := &Server{
		adapter:       adapter,
		audioMgr:      audioMgr,
		snapclientMgr: snapclientMgr,
		alsaAutoRoute: true, // Enable automatic ALSA routing by default
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool {
				// Allow same-origin requests and local network access
				// In production, you may want to restrict this further
				origin := r.Header.Get("Origin")
				if origin == "" {
					return true // No origin header, likely same-origin
				}
				// Allow localhost and local network IPs
				host := r.Host
				if host == "" {
					return false
				}
				return true // For local network devices, allow all origins
			},
		},
		clients:   make(map[*client]bool),
		port:      port,
		tlsConfig: tlsConfig,
	}

	// Set up callback for device changes
	adapter.SetOnChange(s.broadcastDevices)

	return s
}

// Start starts the HTTP server
func (s *Server) Start(ctx context.Context) error {
	mux := http.NewServeMux()

	// Serve static files
	staticFS, err := fs.Sub(staticFiles, "static")
	if err != nil {
		return fmt.Errorf("failed to create static file system: %w", err)
	}
	mux.Handle("/", http.FileServer(http.FS(staticFS)))

	// WebSocket endpoint
	mux.HandleFunc("/ws", s.handleWebSocket)

	// Health check endpoint
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})

	server := &http.Server{
		Addr:      fmt.Sprintf(":%d", s.port),
		Handler:   mux,
		TLSConfig: s.tlsConfig,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		server.Shutdown(shutdownCtx)
	}()

	if s.tlsConfig != nil {
		log.Printf("Starting server on https://0.0.0.0:%d", s.port)
		return server.ListenAndServeTLS("", "")
	}

	log.Printf("Starting server on http://0.0.0.0:%d", s.port)
	return server.ListenAndServe()
}

func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket upgrade error: %v", err)
		return
	}
	defer conn.Close()

	c := &client{conn: conn}

	s.clientsMu.Lock()
	s.clients[c] = true
	s.clientsMu.Unlock()

	defer func() {
		// Stop log streaming if active
		c.logStopFuncMu.Lock()
		if c.logStopFunc != nil {
			c.logStopFunc()
			c.logStopFunc = nil
		}
		c.logStopFuncMu.Unlock()

		s.clientsMu.Lock()
		delete(s.clients, c)
		s.clientsMu.Unlock()
	}()

	// Send initial device list
	s.sendDevices(c)

	// Send ALSA configuration
	s.sendAlsaConfig(c)

	// Send Snapclient status if enabled
	if s.snapclientMgr.IsEnabled() {
		s.sendSnapclientStatus(c)
	}

	// Handle incoming messages
	for {
		_, msgBytes, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("WebSocket error: %v", err)
			}
			break
		}

		var msg Message
		if err := json.Unmarshal(msgBytes, &msg); err != nil {
			s.sendError(c, "Invalid message format")
			continue
		}

		s.handleMessage(c, &msg)
	}
}

func (s *Server) handleMessage(c *client, msg *Message) {
	switch msg.Type {
	case MsgTypeScan:
		log.Println("Received scan request")
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			if err := s.adapter.StartDiscovery(ctx); err != nil {
				s.sendError(c, fmt.Sprintf("Failed to start scan: %v", err))
				return
			}
			s.broadcastStatus("Scanning for devices...", true)
		}()

	case MsgTypeStopScan:
		log.Println("Received stop scan request")
		if err := s.adapter.StopDiscovery(); err != nil {
			s.sendError(c, fmt.Sprintf("Failed to stop scan: %v", err))
			return
		}
		s.broadcastStatus("Scan stopped", false)
		// Also broadcast devices to ensure UI reflects the scanning=false state
		s.broadcastDevices(s.adapter.GetDevices())

	case MsgTypePair:
		var payload DeviceActionPayload
		if err := json.Unmarshal(msg.Payload, &payload); err != nil {
			s.sendError(c, "Invalid payload")
			return
		}
		log.Printf("Received pair request for: %s", payload.Address)
		go func() {
			if err := s.adapter.Pair(payload.Address); err != nil {
				s.sendError(c, fmt.Sprintf("Failed to pair: %v", err))
				return
			}
			s.broadcastStatus(fmt.Sprintf("Paired with %s", payload.Address), s.adapter.IsScanning())
		}()

	case MsgTypeConnect:
		var payload DeviceActionPayload
		if err := json.Unmarshal(msg.Payload, &payload); err != nil {
			s.sendError(c, "Invalid payload")
			return
		}
		log.Printf("Received connect request for: %s", payload.Address)
		go func() {
			if err := s.adapter.Connect(payload.Address); err != nil {
				s.sendError(c, fmt.Sprintf("Failed to connect: %v", err))
				return
			}
			s.broadcastStatus(fmt.Sprintf("Connected to %s", payload.Address), s.adapter.IsScanning())

			// Handle auto-routing and Snapclient restart
			s.handleDeviceConnected(payload.Address)
		}()

	case MsgTypePairAndConnect:
		var payload DeviceActionPayload
		if err := json.Unmarshal(msg.Payload, &payload); err != nil {
			s.sendError(c, "Invalid payload")
			return
		}
		log.Printf("Received pair and connect request for: %s", payload.Address)
		go func() {
			// First pair with the device
			if err := s.adapter.Pair(payload.Address); err != nil {
				s.sendError(c, fmt.Sprintf("Failed to pair: %v", err))
				return
			}
			s.broadcastStatus(fmt.Sprintf("Paired with %s", payload.Address), s.adapter.IsScanning())

			// Then connect to the device
			if err := s.adapter.Connect(payload.Address); err != nil {
				s.sendError(c, fmt.Sprintf("Failed to connect after pairing: %v", err))
				return
			}
			s.broadcastStatus(fmt.Sprintf("Connected to %s", payload.Address), s.adapter.IsScanning())

			// Handle auto-routing and Snapclient restart
			s.handleDeviceConnected(payload.Address)
		}()

	case MsgTypeDisconnect:
		var payload DeviceActionPayload
		if err := json.Unmarshal(msg.Payload, &payload); err != nil {
			s.sendError(c, "Invalid payload")
			return
		}
		log.Printf("Received disconnect request for: %s", payload.Address)
		go func() {
			if err := s.adapter.Disconnect(payload.Address); err != nil {
				s.sendError(c, fmt.Sprintf("Failed to disconnect: %v", err))
				return
			}
			s.broadcastStatus(fmt.Sprintf("Disconnected from %s", payload.Address), s.adapter.IsScanning())
		}()

	case MsgTypeRemove:
		var payload DeviceActionPayload
		if err := json.Unmarshal(msg.Payload, &payload); err != nil {
			s.sendError(c, "Invalid payload")
			return
		}
		log.Printf("Received remove request for: %s", payload.Address)
		go func() {
			if err := s.adapter.Remove(payload.Address); err != nil {
				s.sendError(c, fmt.Sprintf("Failed to remove device: %v", err))
				return
			}
			s.broadcastStatus(fmt.Sprintf("Removed %s", payload.Address), s.adapter.IsScanning())
		}()

	case MsgTypeAlsaGetConfig:
		s.sendAlsaConfig(c)

	case MsgTypeAlsaSetConfig:
		var config AlsaConfig
		if err := json.Unmarshal(msg.Payload, &config); err != nil {
			s.sendError(c, "Invalid ALSA config payload")
			return
		}
		log.Printf("Received ALSA config update: autoRoute=%v", config.AutoRoute)
		s.alsaAutoRouteMu.Lock()
		s.alsaAutoRoute = config.AutoRoute
		s.alsaAutoRouteMu.Unlock()

		// If auto-route is enabled, route to the first connected audio device
		if config.AutoRoute {
			go s.routeToFirstConnectedDevice()
		}
		s.broadcastAlsaConfig()

	case MsgTypeAlsaSetDevice:
		var payload DeviceActionPayload
		if err := json.Unmarshal(msg.Payload, &payload); err != nil {
			s.sendError(c, "Invalid payload")
			return
		}
		log.Printf("Received ALSA set device request for: %s", payload.Address)
		go func() {
			if err := s.audioMgr.SetDefaultDevice(payload.Address); err != nil {
				s.sendError(c, fmt.Sprintf("Failed to set ALSA device: %v", err))
				return
			}
			s.broadcastStatus(fmt.Sprintf("Set %s as default audio output", payload.Address), s.adapter.IsScanning())
			s.broadcastAlsaConfig()
		}()

	case MsgTypeSnapclientGetStatus:
		s.sendSnapclientStatus(c)

	case MsgTypeSnapclientStart:
		go func() {
			if err := s.snapclientMgr.StartService(); err != nil {
				s.sendError(c, fmt.Sprintf("Failed to start Snapclient: %v", err))
				return
			}
			s.sendSnapclientStatus(c)
			s.broadcastStatus("Snapclient service started", s.adapter.IsScanning())
		}()

	case MsgTypeSnapclientStop:
		go func() {
			if err := s.snapclientMgr.StopService(); err != nil {
				s.sendError(c, fmt.Sprintf("Failed to stop Snapclient: %v", err))
				return
			}
			s.sendSnapclientStatus(c)
			s.broadcastStatus("Snapclient service stopped", s.adapter.IsScanning())
		}()

	case MsgTypeSnapclientRestart:
		go func() {
			if err := s.snapclientMgr.RestartService(); err != nil {
				s.sendError(c, fmt.Sprintf("Failed to restart Snapclient: %v", err))
				return
			}
			s.sendSnapclientStatus(c)
			s.broadcastStatus("Snapclient service restarted", s.adapter.IsScanning())
		}()

	case MsgTypeSnapclientSetConfig:
		var config snapcast.Config
		if err := json.Unmarshal(msg.Payload, &config); err != nil {
			s.sendError(c, "Invalid Snapclient config payload")
			return
		}
		go func() {
			if err := s.snapclientMgr.SetConfig(config); err != nil {
				s.sendError(c, fmt.Sprintf("Failed to update Snapclient config: %v", err))
				return
			}
			s.sendSnapclientStatus(c)
			s.broadcastStatus("Snapclient configuration updated", s.adapter.IsScanning())
		}()

	case MsgTypeSnapclientGetPlayers:
		log.Println("Received Snapclient get players request")
		go func() {
			players, err := s.snapclientMgr.ListPCMDevices()
			if err != nil {
				s.sendError(c, fmt.Sprintf("Failed to list players: %v", err))
				return
			}
			s.sendSnapclientPlayers(c, players)
		}()

	case MsgTypeSnapclientGetPCM:
		log.Println("Received Snapclient get PCM devices request")
		go func() {
			devices, err := s.snapclientMgr.ListPCMDevices()
			if err != nil {
				s.sendError(c, fmt.Sprintf("Failed to list PCM devices: %v", err))
				return
			}
			s.sendSnapclientPCMDevices(c, devices)
		}()

	case MsgTypeSnapclientMigrate:
		log.Println("Received Snapclient migration request")
		go func() {
			result := s.snapclientMgr.MigrateToUserService()
			s.sendSnapclientMigrationResult(c, result)
			// Refresh status after migration
			s.sendSnapclientStatus(c)
		}()

	case MsgTypeSnapclientEnableUserService:
		log.Println("Received Snapclient enable user service request")
		go func() {
			result := s.snapclientMgr.EnableUserService()
			s.sendSnapclientEnableResult(c, result)
			// Refresh status after enabling
			s.sendSnapclientStatus(c)
		}()

	case MsgTypeSnapclientSetVolume:
		var payload VolumePayload
		if err := json.Unmarshal(msg.Payload, &payload); err != nil {
			s.sendError(c, "Invalid volume payload")
			return
		}
		log.Printf("Received Snapclient set volume request: %d", payload.Volume)
		go func() {
			// Get current config to check player and soundcard
			config, err := s.snapclientMgr.GetConfig()
			if err != nil {
				s.sendError(c, fmt.Sprintf("Failed to get Snapclient config: %v", err))
				return
			}
			
			// Only set volume if player is "alsa"
			if config.Player != "alsa" {
				s.sendError(c, "Volume control is only available when player is 'alsa'")
				return
			}
			
			// Set the volume
			if err := s.snapclientMgr.SetAlsaVolume(config.Soundcard, payload.Volume); err != nil {
				s.sendError(c, fmt.Sprintf("Failed to set volume: %v", err))
				return
			}
			
			s.broadcastStatus(fmt.Sprintf("Volume set to %d%%", payload.Volume), s.adapter.IsScanning())
			// Refresh status to update UI with new volume
			s.sendSnapclientStatus(c)
		}()

	case MsgTypeSnapclientGetVolume:
		var payload SoundcardPayload
		if err := json.Unmarshal(msg.Payload, &payload); err != nil {
			s.sendError(c, "Invalid soundcard payload")
			return
		}
		log.Printf("Received Snapclient get volume request for soundcard: %s", payload.Soundcard)
		go func() {
			// Get volume for the specified soundcard
			volume, err := s.snapclientMgr.GetAlsaVolume(payload.Soundcard)
			if err != nil {
				log.Printf("Failed to get volume for soundcard %s: %v", payload.Soundcard, err)
				// Send default volume on error
				volume = 100
			}
			
			// Send back volume as part of a volume response
			volumeResponse := VolumePayload{Volume: volume}
			volumeBytes, err := json.Marshal(volumeResponse)
			if err != nil {
				log.Printf("Error marshaling volume response: %v", err)
				return
			}
			
			msg := Message{
				Type:    MsgTypeSnapclientSetVolume,
				Payload: volumeBytes,
			}
			msgBytes, err := json.Marshal(msg)
			if err != nil {
				log.Printf("Error marshaling volume message: %v", err)
				return
			}
			c.mu.Lock()
			c.conn.WriteMessage(websocket.TextMessage, msgBytes)
			c.mu.Unlock()
		}()

	case MsgTypeSnapclientStartLogs:
		log.Println("Received Snapclient start logs request")
		go func() {
			// Stop any existing log stream for this client
			c.logStopFuncMu.Lock()
			if c.logStopFunc != nil {
				c.logStopFunc()
				c.logStopFunc = nil
			}
			c.logStopFuncMu.Unlock()

			// Start streaming logs (get last 100 lines initially)
			ctx, cancel := context.WithCancel(context.Background())
			logChan, stopFunc, err := s.snapclientMgr.StreamLogs(ctx, 100)
			if err != nil {
				cancel()
				s.sendError(c, fmt.Sprintf("Failed to start log stream: %v", err))
				return
			}

			// Store stop function
			c.logStopFuncMu.Lock()
			c.logStopFunc = func() {
				cancel()
				stopFunc()
			}
			c.logStopFuncMu.Unlock()

			// Stream logs to client
			for line := range logChan {
				logPayload := LogPayload{Line: line}
				logBytes, err := json.Marshal(logPayload)
				if err != nil {
					log.Printf("Error marshaling log payload: %v", err)
					continue
				}

				msg := Message{
					Type:    MsgTypeSnapclientLog,
					Payload: logBytes,
				}
				msgBytes, err := json.Marshal(msg)
				if err != nil {
					log.Printf("Error marshaling log message: %v", err)
					continue
				}

				c.mu.Lock()
				err = c.conn.WriteMessage(websocket.TextMessage, msgBytes)
				c.mu.Unlock()
				if err != nil {
					log.Printf("Error sending log message: %v", err)
					break
				}
			}

			// Clean up stop function when done
			c.logStopFuncMu.Lock()
			c.logStopFunc = nil
			c.logStopFuncMu.Unlock()
		}()

	case MsgTypeSnapclientStopLogs:
		log.Println("Received Snapclient stop logs request")
		c.logStopFuncMu.Lock()
		if c.logStopFunc != nil {
			c.logStopFunc()
			c.logStopFunc = nil
		}
		c.logStopFuncMu.Unlock()

	default:
		s.sendError(c, fmt.Sprintf("Unknown message type: %s", msg.Type))
	}
}

func (s *Server) sendDevices(c *client) {
	payload := DevicesPayload{
		Devices:  s.adapter.GetDevices(),
		Scanning: s.adapter.IsScanning(),
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		log.Printf("Error marshaling devices payload: %v", err)
		return
	}
	msg := Message{
		Type:    MsgTypeDevices,
		Payload: payloadBytes,
	}
	msgBytes, err := json.Marshal(msg)
	if err != nil {
		log.Printf("Error marshaling devices message: %v", err)
		return
	}
	c.mu.Lock()
	c.conn.WriteMessage(websocket.TextMessage, msgBytes)
	c.mu.Unlock()
}

func (s *Server) sendError(c *client, errMsg string) {
	payload := ErrorPayload{Message: errMsg}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		log.Printf("Error marshaling error payload: %v", err)
		return
	}
	msg := Message{
		Type:    MsgTypeError,
		Payload: payloadBytes,
	}
	msgBytes, err := json.Marshal(msg)
	if err != nil {
		log.Printf("Error marshaling error message: %v", err)
		return
	}
	c.mu.Lock()
	c.conn.WriteMessage(websocket.TextMessage, msgBytes)
	c.mu.Unlock()
}

func (s *Server) broadcastDevices(devices []*bluetooth.Device) {
	payload := DevicesPayload{
		Devices:  devices,
		Scanning: s.adapter.IsScanning(),
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		log.Printf("Error marshaling broadcast devices payload: %v", err)
		return
	}
	msg := Message{
		Type:    MsgTypeDevices,
		Payload: payloadBytes,
	}
	s.broadcast(&msg)
}

func (s *Server) broadcastStatus(message string, scanning bool) {
	payload := StatusPayload{
		Scanning: scanning,
		Message:  message,
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		log.Printf("Error marshaling status payload: %v", err)
		return
	}
	msg := Message{
		Type:    MsgTypeStatus,
		Payload: payloadBytes,
	}
	s.broadcast(&msg)
}

func (s *Server) sendSnapclientStatus(c *client) {
	status, err := s.snapclientMgr.GetStatus()
	if err != nil {
		log.Printf("Error getting Snapclient status: %v", err)
		return
	}

	statusBytes, err := json.Marshal(status)
	if err != nil {
		log.Printf("Error marshaling Snapclient status: %v", err)
		return
	}

	msg := Message{
		Type:    MsgTypeSnapclientStatus,
		Payload: statusBytes,
	}
	msgBytes, err := json.Marshal(msg)
	if err != nil {
		log.Printf("Error marshaling Snapclient status message: %v", err)
		return
	}
	c.mu.Lock()
	c.conn.WriteMessage(websocket.TextMessage, msgBytes)
	c.mu.Unlock()
}

func (s *Server) sendSnapclientPlayers(c *client, players []snapcast.Player) {
	playersBytes, err := json.Marshal(players)
	if err != nil {
		log.Printf("Error marshaling Snapclient players: %v", err)
		return
	}

	msg := Message{
		Type:    MsgTypeSnapclientPlayers,
		Payload: playersBytes,
	}
	msgBytes, err := json.Marshal(msg)
	if err != nil {
		log.Printf("Error marshaling Snapclient players message: %v", err)
		return
	}
	c.mu.Lock()
	c.conn.WriteMessage(websocket.TextMessage, msgBytes)
	c.mu.Unlock()
}

func (s *Server) sendSnapclientPCMDevices(c *client, devices []snapcast.Player) {
	devicesBytes, err := json.Marshal(devices)
	if err != nil {
		log.Printf("Error marshaling Snapclient PCM devices: %v", err)
		return
	}

	msg := Message{
		Type:    MsgTypeSnapclientPCMDevices,
		Payload: devicesBytes,
	}
	msgBytes, err := json.Marshal(msg)
	if err != nil {
		log.Printf("Error marshaling Snapclient PCM devices message: %v", err)
		return
	}
	c.mu.Lock()
	c.conn.WriteMessage(websocket.TextMessage, msgBytes)
	c.mu.Unlock()
}

func (s *Server) sendSnapclientMigrationResult(c *client, result snapcast.MigrationResult) {
	resultBytes, err := json.Marshal(result)
	if err != nil {
		log.Printf("Error marshaling Snapclient migration result: %v", err)
		return
	}

	msg := Message{
		Type:    MsgTypeSnapclientMigrationResult,
		Payload: resultBytes,
	}
	msgBytes, err := json.Marshal(msg)
	if err != nil {
		log.Printf("Error marshaling Snapclient migration result message: %v", err)
		return
	}
	c.mu.Lock()
	c.conn.WriteMessage(websocket.TextMessage, msgBytes)
	c.mu.Unlock()
}

func (s *Server) sendSnapclientEnableResult(c *client, result snapcast.EnableResult) {
	resultBytes, err := json.Marshal(result)
	if err != nil {
		log.Printf("Error marshaling Snapclient enable result: %v", err)
		return
	}

	msg := Message{
		Type:    MsgTypeSnapclientEnableResult,
		Payload: resultBytes,
	}
	msgBytes, err := json.Marshal(msg)
	if err != nil {
		log.Printf("Error marshaling Snapclient enable result message: %v", err)
		return
	}
	c.mu.Lock()
	c.conn.WriteMessage(websocket.TextMessage, msgBytes)
	c.mu.Unlock()
}

func (s *Server) broadcast(msg *Message) {
	msgBytes, err := json.Marshal(msg)
	if err != nil {
		return
	}

	s.clientsMu.RLock()
	defer s.clientsMu.RUnlock()

	for c := range s.clients {
		c.mu.Lock()
		err := c.conn.WriteMessage(websocket.TextMessage, msgBytes)
		c.mu.Unlock()
		if err != nil {
			log.Printf("Error broadcasting to client: %v", err)
		}
	}
}

func (s *Server) sendAlsaConfig(c *client) {
	s.alsaAutoRouteMu.RLock()
	autoRoute := s.alsaAutoRoute
	s.alsaAutoRouteMu.RUnlock()

	currentDevice, _ := s.audioMgr.GetCurrentDevice()

	config := AlsaConfig{
		AutoRoute:     autoRoute,
		CurrentDevice: currentDevice,
	}

	configBytes, err := json.Marshal(config)
	if err != nil {
		log.Printf("Error marshaling ALSA config: %v", err)
		return
	}

	msg := Message{
		Type:    MsgTypeAlsaConfig,
		Payload: configBytes,
	}
	msgBytes, err := json.Marshal(msg)
	if err != nil {
		log.Printf("Error marshaling ALSA config message: %v", err)
		return
	}
	c.mu.Lock()
	c.conn.WriteMessage(websocket.TextMessage, msgBytes)
	c.mu.Unlock()
}

func (s *Server) broadcastAlsaConfig() {
	s.alsaAutoRouteMu.RLock()
	autoRoute := s.alsaAutoRoute
	s.alsaAutoRouteMu.RUnlock()

	currentDevice, _ := s.audioMgr.GetCurrentDevice()

	config := AlsaConfig{
		AutoRoute:     autoRoute,
		CurrentDevice: currentDevice,
	}

	configBytes, err := json.Marshal(config)
	if err != nil {
		log.Printf("Error marshaling ALSA config: %v", err)
		return
	}

	msg := Message{
		Type:    MsgTypeAlsaConfig,
		Payload: configBytes,
	}
	s.broadcast(&msg)
}

func (s *Server) routeToFirstConnectedDevice() {
	devices := s.adapter.GetDevices()
	for _, device := range devices {
		if device.Connected && audio.IsAudioDevice(device.Icon) {
			log.Printf("Auto-routing audio to first connected device: %s (%s)", device.Name, device.Address)
			if err := s.audioMgr.SetDefaultDevice(device.Address); err != nil {
				log.Printf("Failed to auto-route audio: %v", err)
			} else {
				s.broadcastAlsaConfig()
			}
			return
		}
	}
	log.Println("No connected audio devices found for auto-routing")
}

func (s *Server) handleDeviceConnected(address string) {
	// Check if auto-routing is enabled and route if this is an audio device
	s.alsaAutoRouteMu.RLock()
	autoRoute := s.alsaAutoRoute
	s.alsaAutoRouteMu.RUnlock()

	if autoRoute {
		// Get the device to check if it's an audio device
		devices := s.adapter.GetDevices()
		for _, device := range devices {
			if device.Address == address && audio.IsAudioDevice(device.Icon) {
				log.Printf("Auto-routing audio to newly connected device: %s", address)
				if err := s.audioMgr.SetDefaultDevice(address); err != nil {
					log.Printf("Failed to auto-route audio: %v", err)
				} else {
					s.broadcastAlsaConfig()
				}
				break
			}
		}
	}

	// Restart Snapclient service if enabled
	if s.snapclientMgr != nil {
		log.Println("Restarting Snapclient service after device connection...")
		if err := s.snapclientMgr.RestartService(); err != nil {
			log.Printf("Warning: Failed to restart Snapclient service: %v", err)
		} else {
			log.Println("Snapclient service restarted successfully")
		}
	}
}
