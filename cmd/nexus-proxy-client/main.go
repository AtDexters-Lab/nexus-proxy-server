package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/AtDexters-Lab/nexus-proxy/client"
)

func main() {
	configPath := flag.String("config", "config.yaml", "Path to the configuration file.")
	flag.Parse()

	cfg, err := client.LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("FATAL: Error loading configuration: %v", err)
	}

	log.Printf("INFO: Starting Nexus Backend Client for %d configured services...", len(cfg.Backends))

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	var mu sync.Mutex
	var clients []*client.Client

	for _, backendCfg := range cfg.Backends {
		for i, nexusAddr := range backendCfg.NexusAddresses {
			clientCfg := backendCfg.ToClientConfig(nexusAddr)
			// Only the first client per backend binds the SOCKS5 listener;
			// additional nexus connections share the same backend and would
			// conflict on the same port.
			if i > 0 {
				clientCfg.Socks5ListenAddr = ""
			}
			wg.Add(1)
			go func(cfg client.ClientBackendConfig) {
				defer wg.Done()
				c, err := client.New(cfg)
				if err != nil {
					log.Printf("ERROR: Failed to construct client for backend %s targeting %s: %v", cfg.Name, cfg.NexusAddress, err)
					return
				}
				mu.Lock()
				clients = append(clients, c)
				mu.Unlock()
				c.Start(ctx)
			}(clientCfg)
		}
	}

	shutdownChan := make(chan os.Signal, 1)
	signal.Notify(shutdownChan, syscall.SIGINT, syscall.SIGTERM)
	<-shutdownChan

	log.Println("INFO: Shutdown signal received. Stopping all clients...")
	mu.Lock()
	var stopWg sync.WaitGroup
	for _, c := range clients {
		stopWg.Add(1)
		go func(c *client.Client) {
			defer stopWg.Done()
			c.Stop()
		}(c)
	}
	mu.Unlock()
	stopWg.Wait()
	cancel() // backstop for any clients not yet registered

	wg.Wait()
	log.Println("INFO: All clients have shut down gracefully. Exiting.")
}
