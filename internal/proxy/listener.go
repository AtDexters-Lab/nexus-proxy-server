package proxy

import (
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/AtDexters-Lab/nexus-proxy-server/internal/config"
	"github.com/AtDexters-Lab/nexus-proxy-server/internal/iface"
)

// Listener is responsible for accepting incoming connections from end-users.
type Listener struct {
	config      *config.Config
	hub         iface.Hub
	peerManager iface.PeerManager
	acmeHandler http.Handler // Handler for ACME HTTP-01 challenges
	wg          sync.WaitGroup
	listeners   []net.Listener
	mu          sync.Mutex
}

// NewListener creates a new Listener instance.
func NewListener(cfg *config.Config, hub iface.Hub, pm iface.PeerManager, acme http.Handler) *Listener {
	return &Listener{
		config:      cfg,
		hub:         hub,
		peerManager: pm,
		acmeHandler: acme,
		listeners:   make([]net.Listener, 0, len(cfg.RelayPorts)),
	}
}

// Run starts listeners on all configured proxy ports.
func (l *Listener) Run() {
	for _, port := range l.config.RelayPorts {
		l.wg.Add(1)
		go l.listenOnPort(port)
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
		listener.Close()
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
	peekableConn := NewPeekableConn(conn)
	var hostname string
	var err error

	hostname, err = PeekServerName(peekableConn)
	isTLS := err == nil

	if err != nil {
		log.Printf("INFO: TLS Method: Could not determine hostname for %s: %v", conn.RemoteAddr(), err)
		hostname, err = PeekHost(peekableConn)
		if err == nil {
			// It's an HTTP request. Check if it's for our ACME challenge.
			if l.acmeHandler != nil && strings.EqualFold(hostname, l.config.HubPublicHostname) {
				log.Printf("INFO: Intercepting HTTP request for proxy's own hostname '%s' to handle ACME challenge", hostname)
				simpleHttpServer := &http.Server{Handler: l.acmeHandler}
				simpleHttpServer.Serve(NewSingleConnListener(peekableConn))
				return
			}
		}
	}

	if err != nil {
		log.Printf("WARN: Could not determine hostname for %s: %v. Closing connection.", conn.RemoteAddr(), err)
		conn.Close()
		return
	}

	log.Printf("INFO: Identified request for hostname '%s' from %s (TLS: %v)", hostname, conn.RemoteAddr(), isTLS)

	// First, try to find a local backend.
	backend, err := l.hub.SelectBackend(hostname)
	if err == nil {
		client := NewClient(peekableConn, backend, l.config)
		log.Printf("INFO: [LOCAL] Routing client %s [%s] for hostname '%s' to backend %s", conn.RemoteAddr(), client.id, hostname, backend.ID())
		client.Start()
		return
	}

	// If no local backend, check peers and initiate a tunnel.
	if l.peerManager != nil {
		if remotePeer, ok := l.peerManager.GetPeerForHostname(hostname); ok {
			log.Printf("INFO: [TUNNEL] No local backend for '%s'. Tunneling to peer %s", hostname, remotePeer.Addr())
			remotePeer.StartTunnel(peekableConn, hostname)
			return
		}
	}

	log.Printf("WARN: No local or remote backend available for hostname '%s' for client %s", hostname, conn.RemoteAddr())
	conn.Close()
}
