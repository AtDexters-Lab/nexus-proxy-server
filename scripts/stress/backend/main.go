// Minimal HTTPS backend for nexus-proxy stress testing.
//
// Generates a self-signed cert at startup, serves a tiny "ok" response on
// every path, and is concurrency-safe for high RPS. Pairs with
// nexus-proxy-client to register the host with a nexus dev instance, then
// the stress harness in scripts/stress/run.sh hits the public nexus URL.
//
// Usage:
//
//	go run ./scripts/stress/backend -addr :18443 -hostname stress.example.com
//
// The self-signed cert is fine for stress testing because nexus is a TLS
// passthrough — load testers verify against this backend's cert, not nexus's.
// Use --insecure / -k on the load tester side.
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"flag"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"sync/atomic"
	"time"
)

func main() {
	addr := flag.String("addr", ":18443", "listen address")
	hostname := flag.String("hostname", "stress.local", "subject CN / SAN for the self-signed cert")
	flag.Parse()

	cert, err := makeSelfSigned(*hostname)
	if err != nil {
		log.Fatalf("self-signed cert: %v", err)
	}

	var served atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		served.Add(1)
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Connection", "close") // exercise full connection lifecycle per request
		_, _ = fmt.Fprintln(w, "ok")
	})

	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{cert}},
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		var prev int64
		for range t.C {
			now := served.Load()
			rps := (now - prev) / 10
			prev = now
			log.Printf("served=%d (%d rps over last 10s)", now, rps)
		}
	}()

	log.Printf("stress backend listening on %s (hostname %s)", *addr, *hostname)
	log.Fatal(srv.ListenAndServeTLS("", ""))
}

func makeSelfSigned(hostname string) (tls.Certificate, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: hostname},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(7 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{hostname},
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv}, nil
}
