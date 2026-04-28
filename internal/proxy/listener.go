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
	"sync/atomic"
	"time"

	hn "github.com/AtDexters-Lab/nexus-proxy/hostnames"
	"github.com/AtDexters-Lab/nexus-proxy/internal/config"
	"github.com/AtDexters-Lab/nexus-proxy/internal/iface"
	"github.com/AtDexters-Lab/nexus-proxy/protocol"
)

// truncationWarnInterval throttles ClientHello-truncation WARN logs to at
// most one per interval per Listener; the message includes the count of
// truncations since the last warn so operators don't lose volume signal.
const truncationWarnInterval = 5 * time.Second

// peekDropWarnInterval throttles peek-semaphore-full WARN logs the same way.
const peekDropWarnInterval = 5 * time.Second

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

	// Truncation WARN rate-limit state. truncationCount accumulates between
	// log emissions so a sustained burst surfaces as a single throttled WARN
	// with the cumulative count rather than amplifying into the log stream.
	truncationLastWarnNanos atomic.Int64
	truncationCount         atomic.Int64

	// peekSem bounds in-flight inbound handlers (peek + dispatch). nil when
	// MaxConcurrentPeeks=-1. See gatedHandle for the acquire-inside-goroutine
	// pattern.
	peekSem               chan struct{}
	peekDropLastWarnNanos atomic.Int64
	peekDropCount         atomic.Int64
}

// NewListener creates a new Listener instance.
func NewListener(cfg *config.Config, hub iface.Hub, pm iface.PeerManager, acme http.Handler, acmeTLS *tls.Config) *Listener {
	l := &Listener{
		config:      cfg,
		hub:         hub,
		peerManager: pm,
		acmeHandler: acme,
		acmeTLS:     acmeTLS,
		listeners:   make([]net.Listener, 0, len(cfg.RelayPorts)),
		udpConns:    make([]net.PacketConn, 0, len(cfg.UDPRelayPorts)),
	}
	if limit := cfg.MaxConcurrentPeeksLimit(); limit > 0 {
		l.peekSem = make(chan struct{}, limit)
	}
	return l
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
		go l.gatedHandle(conn)
	}
}

// gatedHandle wraps handleConnection with the peek-concurrency bound. The
// acquire happens inside the goroutine (rather than before `go` in the accept
// loop) so the acquire and the deferred release live in the same goroutine
// scope. If goroutine creation itself fails the slot is never acquired, which
// avoids the silent slot-leak failure mode of acquire-then-spawn.
func (l *Listener) gatedHandle(conn net.Conn) {
	if l.peekSem == nil {
		l.handleConnection(conn)
		return
	}
	select {
	case l.peekSem <- struct{}{}:
		defer func() { <-l.peekSem }()
	default:
		l.warnPeekDrop()
		_ = conn.Close()
		return
	}
	l.handleConnection(conn)
}

// tryClaimWarnSlot bumps the cumulative event count and decides whether the
// caller is the "winner" responsible for emitting the next WARN. Returns
// (snapshot, true) on win — snapshot is the count atomically claimed (and
// reset to zero) for inclusion in the WARN. Returns (0, false) if either
// the throttle window has not elapsed or another goroutine raced to win
// the slot first; in those cases the count keeps accumulating for the next
// emission so no event is lost.
func (l *Listener) tryClaimWarnSlot(count, lastNanos *atomic.Int64, interval time.Duration) (int64, bool) {
	count.Add(1)
	now := time.Now().UnixNano()
	last := lastNanos.Load()
	if now-last < int64(interval) {
		return 0, false
	}
	if !lastNanos.CompareAndSwap(last, now) {
		return 0, false
	}
	return count.Swap(0), true
}

func (l *Listener) warnPeekDrop() {
	emitted, won := l.tryClaimWarnSlot(&l.peekDropCount, &l.peekDropLastWarnNanos, peekDropWarnInterval)
	if !won {
		return
	}
	log.Printf("WARN: peek slots full (cap=%d), dropped %d connections since last warn",
		cap(l.peekSem), emitted)
}

func (l *Listener) warnTruncatedClientHello(conn net.Conn, sni string) {
	emitted, won := l.tryClaimWarnSlot(&l.truncationCount, &l.truncationLastWarnNanos, truncationWarnInterval)
	if !won {
		return
	}
	displaySNI := sni
	if displaySNI == "" {
		displaySNI = "<missing>"
	}
	log.Printf("WARN: ClientHello prelude truncated at peek cap; closing connection from %s SNI=%s (%d truncations since last warn)",
		conn.RemoteAddr(), displaySNI, emitted)
}

func (l *Listener) handleConnection(conn net.Conn) {
	var routeKey string
	var prelude []byte
	var isTLS bool

	localPort := 0
	if tcpAddr, ok := conn.LocalAddr().(*net.TCPAddr); ok {
		localPort = tcpAddr.Port
	}

	// Shared absolute deadline bounds total peek duration per connection,
	// including the TLS-then-HTTP fallback. Prevents per-phase budgets from
	// compounding into 2×AbsoluteMax socket holdtime.
	peekTimeouts := PeekTimeouts{
		FirstByte:     l.config.PeekFirstByteTimeout(),
		IdleExtension: l.config.PeekIdleExtension(),
		AbsDeadline:   time.Now().Add(l.config.PeekAbsoluteMax()),
	}

	// Try TLS SNI first using a robust aborted handshake.
	sni, tlsPrelude, tlsTruncated, tlsErr := PeekSNIAndPrelude(conn, peekTimeouts, 32<<10)
	if tlsTruncated {
		// ClientHello exceeded the recorder cap. Whether SNI was extracted
		// or not, the captured prelude is incomplete and replaying it to a
		// backend or peer would cause a downstream TLS handshake failure
		// the operator cannot easily correlate. Close locally with a
		// rate-limited WARN; the client sees a TCP RST and ops have a
		// single failure point.
		l.warnTruncatedClientHello(conn, sni)
		_ = conn.Close()
		return
	}
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
		host, path, httpPrelude, httpErr := PeekHTTPHostAndPrelude(conn, peekTimeouts, 64<<10)
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

			routeKey = protocol.RouteKey(protocol.TransportTCP, localPort)
			prelude = httpPrelude
			isTLS = tlsDetected
			log.Printf("INFO: No SNI/Host for %s on :%d; attempting port-claim route '%s' (TLS detected: %v)", conn.RemoteAddr(), localPort, routeKey, tlsDetected)
		}
	}

	log.Printf("INFO: Identified request for route '%s' from %s on :%d (TLS: %v)", routeKey, conn.RemoteAddr(), localPort, isTLS)

	// First, try to find a local backend.
	backend, err := l.hub.SelectBackend(routeKey)
	if err == nil {
		// Wrap in PausableConn for flow control support
		pausableConn := NewPausableConn(conn)
		client := NewClientWithPrelude(pausableConn, backend, l.config, routeKey, prelude, isTLS)
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
