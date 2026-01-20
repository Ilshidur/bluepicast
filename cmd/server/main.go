package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"flag"
	"fmt"
	"log"
	"math/big"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Ilshidur/bluepicast/internal/audio"
	"github.com/Ilshidur/bluepicast/internal/bluetooth"
	"github.com/Ilshidur/bluepicast/internal/snapcast"
	"github.com/Ilshidur/bluepicast/internal/web"
)

func main() {
	port := flag.Int("port", 80, "HTTP server port")
	enableSnapclient := flag.Bool("enable-systemd-snapclient", false, "Enable Snapclient integration for managing Snapcast client")
	enableHTTPS := flag.Bool("https", false, "Enable HTTPS with a self-signed certificate")
	flag.Parse()

	log.Println("BluePiCast")
	log.Println("==========")

	// Initialize Bluetooth adapter
	adapter, err := bluetooth.NewAdapter()
	if err != nil {
		log.Fatalf("Failed to initialize Bluetooth adapter: %v", err)
	}
	defer adapter.Close()

	log.Println("Bluetooth adapter initialized successfully")

	// Initialize audio manager for ALSA routing
	audioManager := audio.NewManager()

	// Initialize Snapclient manager if enabled
	snapclientManager := snapcast.NewManager(*enableSnapclient)
	if *enableSnapclient {
		log.Println("Snapclient integration enabled")
	}

	// Create context with cancellation
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle shutdown signals
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-sigChan
		log.Printf("Received signal %v, shutting down...", sig)
		cancel()
	}()

	// Generate TLS config if HTTPS is enabled
	var tlsConfig *tls.Config
	if *enableHTTPS {
		var err error
		tlsConfig, err = generateSelfSignedTLSConfig()
		if err != nil {
			log.Fatalf("Failed to generate TLS config: %v", err)
		}
		log.Println("HTTPS enabled with self-signed certificate")
	}

	// Start web server
	server := web.NewServer(adapter, audioManager, snapclientManager, *port, tlsConfig)
	if err := server.Start(ctx); err != nil {
		if err != context.Canceled && err.Error() != "http: Server closed" {
			log.Fatalf("Server error: %v", err)
		}
	}

	log.Println("Shutdown complete")
}

// generateSelfSignedTLSConfig generates a self-signed TLS certificate and returns a tls.Config
func generateSelfSignedTLSConfig() (*tls.Config, error) {
	// Generate ECDSA private key
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("failed to generate private key: %w", err)
	}

	// Create certificate template
	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("failed to generate serial number: %w", err)
	}

	notBefore := time.Now()
	notAfter := notBefore.Add(10 * 365 * 24 * time.Hour) // Valid for 10 years

	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{"BluePiCast"},
			CommonName:   "BluePiCast Self-Signed",
		},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("0.0.0.0")},
		DNSNames:              []string{"localhost", "bluepicast", "bluepicast.local"},
	}

	// Add local network IPs
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, addr := range addrs {
			if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
				if ipnet.IP.To4() != nil {
					template.IPAddresses = append(template.IPAddresses, ipnet.IP)
				}
			}
		}
	}

	// Create certificate
	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &privateKey.PublicKey, privateKey)
	if err != nil {
		return nil, fmt.Errorf("failed to create certificate: %w", err)
	}

	// Encode certificate to PEM
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})

	// Encode private key to PEM
	keyDER, err := x509.MarshalECPrivateKey(privateKey)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal private key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	// Create TLS certificate
	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("failed to create TLS certificate: %w", err)
	}

	return &tls.Config{
		Certificates: []tls.Certificate{tlsCert},
		MinVersion:   tls.VersionTLS12,
	}, nil
}
