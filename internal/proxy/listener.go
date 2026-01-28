package proxy

import (
	"crypto/tls"
	"errors"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/AtDexters-Lab/nexus-proxy-server/internal/config"
	hn "github.com/AtDexters-Lab/nexus-proxy-server/internal/hostnames"
	"github.com/AtDexters-Lab/nexus-proxy-server/internal/iface"
)

// Listener is responsible for accepting incoming connections from end-users.
type Listener struct {
	config      *config.Config
	hub         iface.Hub
	peerManager iface.PeerManager
	acmeHandler http.Handler // Handler for ACME HTTP-01 challenges
	acmeTLS     *tls.Config  // TLS config to satisfy TLS-ALPN-01 on :443
	wg          sync.WaitGroup
	listeners   []net.Listener
	udpConns    []net.PacketConn
	mu          sync.Mutex
}

// NewListener creates a new Listener instance.
func NewListener(cfg *config.Config, hub iface.Hub, pm iface.PeerManager, acme http.Handler, acmeTLS *tls.Config) *Listener {
	return &Listener{
		config:      cfg,
		hub:         hub,
		peerManager: pm,
		acmeHandler: acme,
		acmeTLS:     acmeTLS,
		listeners:   make([]net.Listener, 0, len(cfg.RelayPorts)),
		udpConns:    make([]net.PacketConn, 0, len(cfg.UDPRelayPorts)),
	}
}

// Run starts listeners on all configured proxy ports.
func (l *Listener) Run() {
	for _, port := range l.config.RelayPorts {
		l.wg.Add(1)
		go l.listenOnPort(port)
	}
	for _, port := range l.config.UDPRelayPorts {
		l.wg.Add(1)
		go l.listenOnUDPPort(port)
	}
	l.wg.Wait()
	log.Println("INFO: All public listeners have stopped.")
}

// Stop gracefully closes all active network listeners.
func (l *Listener) Stop() {
	log.Println("INFO: Stopping public listeners...")
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, listener := range l.listeners {
		_ = listener.Close()
	}
	for _, pc := range l.udpConns {
		_ = pc.Close()
	}
}

func (l *Listener) listenOnPort(port int) {
	defer l.wg.Done()
	listenAddr := ":" + strconv.Itoa(port)
	tcpListener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		log.Fatalf("ERROR: Failed to start listener on port %d: %v", port, err)
		return
	}
	l.mu.Lock()
	l.listeners = append(l.listeners, tcpListener)
	l.mu.Unlock()
	log.Printf("INFO: Public listener started on %s", listenAddr)
	for {
		conn, err := tcpListener.Accept()
		if err != nil {
			if opErr, ok := err.(*net.OpError); ok && strings.Contains(opErr.Err.Error(), "use of closed network connection") {
				return
			}
			log.Printf("ERROR: Failed to accept new connection on port %d: %v", port, err)
			continue
		}
		go l.handleConnection(conn)
	}
}

func (l *Listener) handleConnection(conn net.Conn) {
	var routeKey string
	var prelude []byte
	var isTLS bool

	localPort := 0
	if tcpAddr, ok := conn.LocalAddr().(*net.TCPAddr); ok {
		localPort = tcpAddr.Port
	}

	// Try TLS SNI first using a robust aborted handshake.
	sni, tlsPrelude, tlsErr := PeekSNIAndPrelude(conn, 5*time.Second, 32<<10)
	tlsDetected := errors.Is(tlsErr, ErrMissingSNI)
	if tlsErr == nil && sni != "" {
		routeKey = hn.Normalize(sni)
		prelude = tlsPrelude
		isTLS = true
	} else {
		// Reinstate any bytes read during TLS sniff before attempting HTTP.
		if len(tlsPrelude) > 0 {
			conn = WithPrelude(conn, tlsPrelude)
		}
		// Fallback to HTTP Host sniffing on plaintext.
		host, path, httpPrelude, httpErr := PeekHTTPHostAndPrelude(conn, 5*time.Second, 64<<10)
		if httpErr == nil && host != "" {
			routeKey = host
			prelude = httpPrelude
			isTLS = false
			// Check if it's for our ACME HTTP-01 challenge.
			hubHostNorm := hn.Normalize(l.config.HubPublicHostname)
			if l.acmeHandler != nil && routeKey == hubHostNorm && localPort == 80 && strings.HasPrefix(path, "/.well-known/acme-challenge/") {
				log.Printf("INFO: Intercepting HTTP request for proxy's own hostname '%s' on :80 to handle ACME challenge", routeKey)
				simpleHttpServer := &http.Server{Handler: l.acmeHandler, ReadHeaderTimeout: 5 * time.Second}
				// Reinsert the bytes we consumed into the stream for the HTTP server.
				connWithPrelude := WithPrelude(conn, prelude)
				err := simpleHttpServer.Serve(NewSingleConnListener(connWithPrelude))
				log.Printf("INFO: ACME HTTP handler finished for '%s' on :80: %v", routeKey, err)
				return
			}
		} else {
			// Neither TLS (with SNI) nor HTTP (with Host). Attempt a TCP port-claim route.
			if len(httpPrelude) > 0 {
				previewLen := len(httpPrelude)
				if previewLen > 24 {
					previewLen = 24
				}
				log.Printf("DEBUG: HTTP sniff read %d bytes on :%d (hex preview %x)", len(httpPrelude), localPort, httpPrelude[:previewLen])
			}
			if errors.Is(httpErr, ErrHTTPPreludeTooLarge) {
				log.Printf("WARN: HTTP prelude exceeded limit for %s on :%d; dropping connection", conn.RemoteAddr(), localPort)
				_ = conn.Close()
				return
			}

			routeKey = "tcp:" + strconv.Itoa(localPort)
			prelude = httpPrelude
			isTLS = tlsDetected
			log.Printf("INFO: No SNI/Host for %s on :%d; attempting port-claim route '%s' (TLS detected: %v)", conn.RemoteAddr(), localPort, routeKey, tlsDetected)
		}
	}

	log.Printf("INFO: Identified request for route '%s' from %s on :%d (TLS: %v)", routeKey, conn.RemoteAddr(), localPort, isTLS)

	// First, try to find a local backend.
	backend, err := l.hub.SelectBackend(routeKey)
	if err == nil {
		// Forward the prelude first, then stream the rest.
		client := NewClientWithPrelude(conn, backend, l.config, routeKey, prelude, isTLS)
		log.Printf("INFO: [LOCAL] Routing client %s [%s] for route '%s' to backend %s", conn.RemoteAddr(), client.id, routeKey, backend.ID())
		client.Start()
		return
	}

	// If no local backend, check peers and initiate a tunnel.
	if l.peerManager != nil {
		if remotePeer, ok := l.peerManager.GetPeerForHostname(routeKey); ok {
			log.Printf("INFO: [TUNNEL] No local backend for '%s'. Tunneling to peer %s", routeKey, remotePeer.Addr())
			// Ensure the tunneled peer sees the bytes we consumed during sniffing.
			connWithPrelude := WithPrelude(conn, prelude)
			remotePeer.StartTunnel(connWithPrelude, routeKey, isTLS)
			return
		}
	}

	log.Printf("WARN: No local or remote backend available for route '%s' for client %s", routeKey, conn.RemoteAddr())
	_ = conn.Close()
}
