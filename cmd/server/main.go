package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/Ilshidur/bluepicast/internal/audio"
	"github.com/Ilshidur/bluepicast/internal/bluetooth"
	"github.com/Ilshidur/bluepicast/internal/snapcast"
	"github.com/Ilshidur/bluepicast/internal/web"
)

func main() {
	port := flag.Int("port", 80, "HTTP server port")
	enableSnapclient := flag.Bool("enable-systemd-snapclient", false, "Enable Snapclient integration for managing Snapcast client")
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

	// Start web server
	server := web.NewServer(adapter, audioManager, snapclientManager, *port)
	if err := server.Start(ctx); err != nil {
		if err != context.Canceled && err.Error() != "http: Server closed" {
			log.Fatalf("Server error: %v", err)
		}
	}

	log.Println("Shutdown complete")
}
