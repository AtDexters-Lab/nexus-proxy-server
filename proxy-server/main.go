package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/AtDexters-Lab/nexus-proxy-server/internal/config"
	"github.com/AtDexters-Lab/nexus-proxy-server/internal/hub"
	"github.com/AtDexters-Lab/nexus-proxy-server/internal/peer"
	"github.com/AtDexters-Lab/nexus-proxy-server/internal/proxy"
)

func main() {
	// --- 1. Configuration Loading ---
	configPath := flag.String("config", "config.yaml", "Path to the configuration file.")
	flag.Parse()

	cfg, err := config.LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("FATAL: Error loading configuration: %v", err)
	}

	log.Printf("INFO: Configuration loaded successfully from %s", *configPath)
	log.Printf("INFO: Hub Address: %s", cfg.BackendListenAddress)
	log.Printf("INFO: Public Ports: %v", cfg.RelayPorts)
	if cfg.IdleTimeout() > 0 {
		log.Printf("INFO: Client idle timeout is %s", cfg.IdleTimeout())
	}

	// --- 2. Server Initialization ---
	log.Println("INFO: Server initialization sequence starting...")

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup

	// We must initialize components in an order that allows dependency injection
	// while avoiding circular dependencies.
	backendHub := hub.New(cfg)
	peerManager := peer.NewManager(cfg, backendHub)
	clientListener := proxy.NewListener(cfg, backendHub, peerManager)

	// Now that all components are created, inject the peer manager into the hub.
	backendHub.SetPeerManager(peerManager)

	// Run all components in goroutines.
	wg.Add(1)
	go func() {
		defer wg.Done()
		backendHub.Run()
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		clientListener.Run()
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		peerManager.Run(ctx)
	}()

	// --- 3. Graceful Shutdown ---
	shutdownChan := make(chan os.Signal, 1)
	signal.Notify(shutdownChan, syscall.SIGINT, syscall.SIGTERM)

	log.Println("INFO: Nexus Proxy is running. Press CTRL+C to exit.")

	<-shutdownChan
	log.Println("INFO: Shutdown signal received.")

	// --- 4. Cleanup ---
	log.Println("INFO: Initiating graceful shutdown...")

	// Stop the peer manager first to prevent new tunneled connections.
	peerManager.Stop()

	// Then, stop the public-facing listeners.
	clientListener.Stop()

	// Finally, stop the backend hub.
	backendHub.Stop()

	// Cancel the main context to signal all other goroutines.
	cancel()

	// Wait for all main goroutines to finish their work.
	wg.Wait()

	log.Println("INFO: Shutdown complete. Goodbye.")
}
