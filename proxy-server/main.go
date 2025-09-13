package main

import (
	"context"
	"crypto/tls"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/AtDexters-Lab/nexus-proxy-server/internal/config"
	"github.com/AtDexters-Lab/nexus-proxy-server/internal/hub"
	"github.com/AtDexters-Lab/nexus-proxy-server/internal/peer"
	"github.com/AtDexters-Lab/nexus-proxy-server/internal/proxy"
	"golang.org/x/crypto/acme/autocert"
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

	// --- 2. Server Initialization ---
	log.Println("INFO: Server initialization sequence starting...")

	var acmeHandler http.Handler
	var hubTlsConfig *tls.Config

	if cfg.HubPublicHostname != "" {
		log.Println("INFO: Hub TLS mode: Automatic (Let's Encrypt using HTTP-01)")

		cacheDir := cfg.AcmeCacheDir
		if cacheDir == "" {
			cacheDir = "acme_certs"
		}
		if err := os.MkdirAll(cacheDir, 0700); err != nil {
			log.Fatalf("FATAL: Could not create ACME cache directory %s: %v", cacheDir, err)
		}
		log.Printf("INFO: ACME certificate cache directory: %s", cacheDir)

		certManager := &autocert.Manager{
			Prompt:     autocert.AcceptTOS,
			HostPolicy: autocert.HostWhitelist(cfg.HubPublicHostname),
			Cache:      autocert.DirCache(cacheDir),
		}

		hubTlsConfig = certManager.TLSConfig()
		acmeHandler = certManager.HTTPHandler(nil)

	} else {
		log.Println("INFO: Hub TLS mode: Manual (from file)")
		cert, err := tls.LoadX509KeyPair(cfg.HubTlsCertFile, cfg.HubTlsKeyFile)
		if err != nil {
			log.Fatalf("FATAL: Failed to load manual TLS certificates: %v", err)
		}
		hubTlsConfig = &tls.Config{Certificates: []tls.Certificate{cert}}
	}

	log.Printf("INFO: Public Ports: %v", cfg.RelayPorts)
	if cfg.IdleTimeout() > 0 {
		log.Printf("INFO: Client idle timeout is %s", cfg.IdleTimeout())
	}

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup

	backendHub := hub.New(cfg, hubTlsConfig)
	peerManager := peer.NewManager(cfg, backendHub)
	clientListener := proxy.NewListener(cfg, backendHub, peerManager, acmeHandler)

	backendHub.SetPeerManager(peerManager)

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
