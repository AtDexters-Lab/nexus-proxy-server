package peer

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AtDexters-Lab/nexus-proxy/internal/config"
	"github.com/AtDexters-Lab/nexus-proxy/internal/netutil"
	"github.com/AtDexters-Lab/nexus-proxy/internal/proxy"
	"github.com/AtDexters-Lab/nexus-proxy/protocol"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

const (
	writeWait            = 10 * time.Second
	pongWait             = 60 * time.Second
	pingPeriod           = (pongWait * 9) / 10
	reconnectDelay       = 5 * time.Second
	tcpCloseGracePeriod  = 2 * time.Second
)

// tunnelState tracks per-tunnel credit state at the origin peer.
type tunnelState struct {
	sendCh       chan struct{} // direction 1 credits: origin → destination
	sendActive   atomic.Bool  // true after first PeerTunnelCredits received
	recvConsumed atomic.Int32 // direction 2 consumption counter for replenishment
	done         chan struct{} // closed when tunnel should exit (e.g. PeerTunnelClose received)
	doneOnce     sync.Once
}

type peerImpl struct {
	addr            string
	conn            *websocket.Conn
	config          *config.Config
	manager         *Manager
	send            chan []byte
	activeTunnels   sync.Map
	tunnelHostnames sync.Map // clientID (uuid.UUID) → hostname (string) for bandwidth tracking
	tunnelStates    sync.Map // clientID (uuid.UUID) → *tunnelState (credit tracking at origin)
	peerDone        atomic.Pointer[chan struct{}] // closed when handleConnection exits; detects peer death
}

// NewPeer creates a new Peer instance.
func NewPeer(addr string, cfg *config.Config, mgr *Manager) *peerImpl {
	return &peerImpl{
		addr:    addr,
		config:  cfg,
		manager: mgr,
		send:    make(chan []byte, 256),
	}
}

func (p *peerImpl) Addr() string {
	return p.addr
}

// Connect attempts to establish an outbound mTLS WebSocket connection to the peer.
func (p *peerImpl) Connect(ctx context.Context) {
	// Prepare TLS config for client auth (mTLS).
	tlsConfig := &tls.Config{}

	// Prefer automatic TLS if configured (uses the same autocert-managed cert as the hub servers).
	if p.config.HubPublicHostname != "" && p.manager != nil && p.manager.tlsBase != nil {
		base := p.manager.tlsBase
		if base.GetCertificate != nil {
			// Dynamically fetch cert at handshake time so renewals are picked up.
			tlsConfig.GetClientCertificate = func(_ *tls.CertificateRequestInfo) (*tls.Certificate, error) {
				return base.GetCertificate(&tls.ClientHelloInfo{ServerName: p.config.HubPublicHostname})
			}
		} else if len(base.Certificates) > 0 {
			// Fallback: reuse the first loaded certificate.
			tlsConfig.Certificates = base.Certificates
		}
	} else {
		// Manual TLS mode: load from configured files.
		cert, err := tls.LoadX509KeyPair(p.config.HubTlsCertFile, p.config.HubTlsKeyFile)
		if err != nil {
			log.Printf("FATAL: [PEER] Failed to load client certificate for mTLS dialing to %s: %v. This peer connection will not be established.", p.addr, err)
			return
		}
		tlsConfig.Certificates = []tls.Certificate{cert}
	}
	dialer := websocket.Dialer{
		Proxy:            http.ProxyFromEnvironment,
		HandshakeTimeout: 15 * time.Second,
		TLSClientConfig:  tlsConfig,
	}

	for {
		select {
		case <-ctx.Done():
			return
		default:
			log.Printf("INFO: [PEER] Attempting to connect to %s via mTLS", p.addr)
			// Use the custom dialer and pass nil for headers.
			conn, _, err := dialer.Dial(p.addr, nil)
			if err != nil {
				log.Printf("WARN: [PEER] Failed to connect to %s: %v. Retrying in %s...", p.addr, err, reconnectDelay)
				time.Sleep(reconnectDelay)
				continue
			}

			p.conn = conn
			p.handleConnection(ctx)
			p.conn = nil
		}
	}
}

// handleConnection starts the read/write pumps for the peer connection.
func (p *peerImpl) handleConnection(ctx context.Context) {
	// peerDone is closed when this connection ends, allowing StartTunnel
	// goroutines to detect peer death and unblock credit waits.
	done := make(chan struct{})
	p.peerDone.Store(&done)
	defer close(done)

	var wg sync.WaitGroup
	wg.Add(2)

	log.Printf("INFO: [PEER] Connection to %s is now active.", p.addr)
	defer p.manager.ClearRoutesForPeer(p)
	defer log.Printf("INFO: [PEER] Connection to %s has been terminated.", p.addr)

	// Upon connecting, immediately announce our current routes to the new peer.
	p.manager.AnnounceLocalRoutes()

	go func() {
		defer wg.Done()
		p.writePump(ctx)
	}()
	go func() {
		defer wg.Done()
		p.readPump()
	}()

	wg.Wait()
}

// Send queues a message to be sent to the peer.
// Returns true if the message was enqueued, false if it was dropped.
func (p *peerImpl) Send(message []byte) bool {
	// A non-blocking send to the channel.
	select {
	case p.send <- message:
		return true
	default:
		log.Printf("WARN: [PEER] Send channel for peer %s is full. Dropping message.", p.addr)
		return false
	}
}

func (p *peerImpl) readPump() {
	defer p.conn.Close()

	for {
		msgType, message, err := p.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("ERROR: [PEER] Unexpected close error from %s: %v", p.addr, err)
			}
			break
		}

		// Handle different message types based on WebSocket frame type.
		switch msgType {
		case websocket.TextMessage:
			// Control messages are sent as JSON text.
			var msg protocol.PeerMessage
			if err := json.Unmarshal(message, &msg); err != nil {
				log.Printf("WARN: [PEER] Failed to unmarshal text message from %s: %v", p.addr, err)
				continue
			}

			switch msg.Type {
			case protocol.PeerAnnounce:
				p.manager.UpdatePeerRoutes(p, msg.Version, msg.Hostnames)
			case protocol.PeerTunnelRequest:
				log.Println("INFO: [PEER] Received tunnel request from", p.addr, "for client", msg.ClientID, "on hostname", msg.Hostname, "with IP", msg.ClientIP, "and port", msg.ConnPort, "(TLS:", msg.IsTLS, ")")
				p.manager.HandleTunnelRequest(p, msg.Hostname, msg.ClientID, msg.ClientIP, msg.ConnPort, msg.IsTLS)
			default:
				log.Printf("WARN: [PEER] Received unknown text message type from %s", p.addr)
			}

		case websocket.BinaryMessage:
			// Data messages are binary for efficiency.
			if len(message) < 1 {
				continue
			}

			controlByte := message[0]
			switch controlByte {
			case protocol.PeerTunnelData:
				if len(message) < 1+protocol.ClientIDLength {
					log.Printf("WARN: [PEER] Received tunnel data with insufficient length from %s", p.addr)
					continue
				}
				var clientID uuid.UUID
				copy(clientID[:], message[1:1+protocol.ClientIDLength])
				payload := message[1+protocol.ClientIDLength:]

				if rawConn, ok := p.activeTunnels.Load(clientID); ok {
					if clientConn, ok := rawConn.(net.Conn); ok {
						// Bandwidth check with retry loop BEFORE writing to client (counted at origin)
						bandwidthScheduler := p.manager.GetBandwidthScheduler()
						if bandwidthScheduler != nil {
							if hostnameRaw, ok := p.tunnelHostnames.Load(clientID); ok {
								hostname := hostnameRaw.(string)
								backendID := "tunnel:" + hostname
							bandwidthLoop:
								for {
									// Use full message size (header + payload) for consistent accounting with outbound path
									allowed, waitTime := bandwidthScheduler.RequestSend(backendID, len(message))
									if allowed {
										break
									}
									// Wait with shutdown handling
									select {
									case <-p.manager.Done():
										return // Manager shutting down
									case <-time.After(waitTime):
										continue bandwidthLoop
									}
								}
							}
						}

						if _, err := clientConn.Write(payload); err != nil {
							log.Printf("WARN: [PEER-TUNNEL] Failed to write back to client %s: %v. Closing connection.", clientID, err)
							netutil.GracefulCloseConn(clientConn, p.manager.Done(), tcpCloseGracePeriod)
						} else {
							// Record sent bytes
							if bandwidthScheduler != nil {
								if hostnameRaw, ok := p.tunnelHostnames.Load(clientID); ok {
									hostname := hostnameRaw.(string)
									backendID := "tunnel:" + hostname
									bandwidthScheduler.RecordSent(backendID, len(message))
								}
							}
							// Direction 2 credit replenishment: we consumed a
							// message from the destination, send credits back
							// so the destination can keep sending.
							if raw, ok := p.tunnelStates.Load(clientID); ok {
								st := raw.(*tunnelState)
								if consumed := st.recvConsumed.Add(1); consumed >= int32(protocol.CreditReplenishBatch) {
									actual := st.recvConsumed.Swap(0)
									msg := makeTunnelCreditMessage(clientID, int64(actual))
									if !p.Send(msg) {
										// Channel full — retry async. Without this,
										// the destination blocks on credits with no retrigger.
										go retryCreditSend(p, msg, p.manager.Done())
									}
								}
							}
						}
					}
				} else {
					// Otherwise, it may be an outbound UDP flow. If not, treat as inbound tunnel data.
					if p.manager.HandleOutboundUDPData(clientID, payload) {
						continue
					}
					// Otherwise, it's for an inbound tunnel. Let the manager handle it.
					p.manager.HandleTunnelData(clientID, payload)
				}

			case protocol.PeerTunnelClose:
				if len(message) < 1+protocol.ClientIDLength {
					continue
				}
				var clientID uuid.UUID
				copy(clientID[:], message[1:1+protocol.ClientIDLength])

				if rawConn, ok := p.activeTunnels.Load(clientID); ok {
					if clientConn, ok := rawConn.(net.Conn); ok {
						log.Printf("INFO: [PEER-TUNNEL] Peer signaled close for our outbound tunnel %s. Closing client connection.", clientID)
						clientConn.Close() // Immediate close — peer explicitly signaled tunnel end.
						p.activeTunnels.Delete(clientID)
						// Signal tunnelState.done to unblock StartTunnel if it's
						// waiting on credit acquisition.
						if raw, ok := p.tunnelStates.Load(clientID); ok {
							raw.(*tunnelState).doneOnce.Do(func() { close(raw.(*tunnelState).done) })
						}
					}
				} else {
					// Otherwise, it may be an outbound UDP flow.
					if p.manager.HandleOutboundUDPClose(clientID) {
						continue
					}
					// Otherwise, it's for an inbound tunnel.
					p.manager.HandleTunnelClose(clientID)
				}

			case protocol.PeerTunnelPause:
				if len(message) < 1+protocol.ClientIDLength {
					continue
				}
				var clientID uuid.UUID
				copy(clientID[:], message[1:1+protocol.ClientIDLength])
				if rawConn, ok := p.activeTunnels.Load(clientID); ok {
					if pausable, ok := rawConn.(interface{ Pause() }); ok {
						pausable.Pause()
						log.Printf("DEBUG: [PEER] Paused tunnel for client %s from %s", clientID, p.addr)
					}
				} else {
					log.Printf("WARN: [PEER] pause_tunnel for unknown client %s (ignored)", clientID)
				}

			case protocol.PeerTunnelResume:
				if len(message) < 1+protocol.ClientIDLength {
					continue
				}
				var clientID uuid.UUID
				copy(clientID[:], message[1:1+protocol.ClientIDLength])
				if rawConn, ok := p.activeTunnels.Load(clientID); ok {
					if pausable, ok := rawConn.(interface{ Resume() }); ok {
						pausable.Resume()
						log.Printf("DEBUG: [PEER] Resumed tunnel for client %s from %s", clientID, p.addr)
					}
				} else {
					log.Printf("WARN: [PEER] resume_tunnel for unknown client %s (ignored)", clientID)
				}

			case protocol.PeerTunnelCredits:
				if len(message) < 1+protocol.ClientIDLength+8 {
					log.Printf("WARN: [PEER] Received tunnel credits with insufficient length from %s", p.addr)
					continue
				}
				var clientID uuid.UUID
				copy(clientID[:], message[1:1+protocol.ClientIDLength])
				credits := int64(binary.BigEndian.Uint64(message[1+protocol.ClientIDLength:]))
				// Cap to prevent CPU-burn DoS from a malicious peer sending
				// a huge credit value (the loop below would spin billions of
				// times on the readPump goroutine).
				if credits <= 0 || credits > protocol.DefaultCreditCapacity {
					credits = protocol.DefaultCreditCapacity
				}

				// Origin side: credits for direction 1 (we can send more to destination).
				if raw, ok := p.tunnelStates.Load(clientID); ok {
					st := raw.(*tunnelState)
					if !st.sendActive.Load() {
						st.sendActive.Store(true)
					}
					for i := int64(0); i < credits; i++ {
						select {
						case st.sendCh <- struct{}{}:
						default:
						}
					}
					continue
				}
				// Destination side: credits for direction 2 (TunneledConn can send more to origin).
				p.manager.AddTunnelCredits(clientID, credits)

			default:
				log.Printf("WARN: [PEER] Received unknown binary control byte from %s", p.addr)
			}
		}
	}
}

func (p *peerImpl) writePump(ctx context.Context) {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		p.conn.Close()
	}()
	for {
		select {
		case message, ok := <-p.send:
			p.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				p.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			// Determine message type by trying to unmarshal it as JSON.
			var jsonCheck map[string]interface{}
			msgType := websocket.BinaryMessage
			if json.Unmarshal(message, &jsonCheck) == nil {
				msgType = websocket.TextMessage
			}

			if err := p.conn.WriteMessage(msgType, message); err != nil {
				log.Printf("ERROR: [PEER] Failed to write to %s: %v", p.addr, err)
				return
			}
		case <-ticker.C:
			p.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := p.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		case <-ctx.Done():
			p.conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
			return
		}
	}
}

// StartTunnel initiates the tunneling of a client connection to target peer.
func (p *peerImpl) StartTunnel(conn net.Conn, hostname string, isTLS bool) {
	defer netutil.GracefulCloseConn(conn, p.manager.Done(), tcpCloseGracePeriod)

	clientID := uuid.New()

	clientIp := conn.RemoteAddr().String()
	var connPort int
	if tcpAddr, ok := conn.LocalAddr().(*net.TCPAddr); ok {
		connPort = tcpAddr.Port
	} else {
		// Handle cases where it might not be a TCP connection, though it always should be.
		log.Printf("WARN: [PEER] Unable to determine local port for connection from %s. Using default port 0.", clientIp)
		return
	}

	// 1. Send the tunnel request to the peer (as a JSON text message).
	req := protocol.PeerMessage{
		Type:      protocol.PeerTunnelRequest,
		Hostname:  hostname,
		ClientID:  clientID,
		ConnPort:  connPort,
		ClientIP:  clientIp,
		Transport: protocol.TransportTCP,
		IsTLS:     isTLS,
	}
	payload, _ := json.Marshal(req)
	p.Send(payload)

	// Capture peer-done at entry so we can detect peer WebSocket death
	// while blocked on credit acquisition.
	var peerDone <-chan struct{}
	if ptr := p.peerDone.Load(); ptr != nil {
		peerDone = *ptr
	}

	// Wrap in PausableConn for flow control
	pausableConn := proxy.NewPausableConn(conn)
	p.activeTunnels.Store(clientID, pausableConn)
	p.tunnelHostnames.Store(clientID, hostname) // Track hostname for bandwidth accounting
	defer p.activeTunnels.Delete(clientID)
	defer p.tunnelHostnames.Delete(clientID)

	// Initialize credit state for this tunnel (origin side).
	state := &tunnelState{
		sendCh: make(chan struct{}, protocol.DefaultCreditCapacity),
		done:   make(chan struct{}),
	}
	p.tunnelStates.Store(clientID, state)
	defer p.tunnelStates.Delete(clientID)

	// Grant initial credits for direction 2 (destination → origin) so the
	// destination's TunneledConn.Write() can start sending response data.
	p.Send(makeTunnelCreditMessage(clientID, protocol.DefaultCreditCapacity))

	// Get bandwidth scheduler for this tunnel and register the synthetic backend
	// Use RegisterShared for reference counting since multiple tunnels may share the same hostname
	bandwidthScheduler := p.manager.GetBandwidthScheduler()
	if bandwidthScheduler != nil {
		backendID := "tunnel:" + hostname
		bandwidthScheduler.RegisterShared(backendID)
		defer bandwidthScheduler.UnregisterShared(backendID)
	}

	// 2. Start proxying data from the client to the peer (as binary messages).
	bufPtr := proxy.GetBuffer()
	defer proxy.PutBuffer(bufPtr)
	buf := *bufPtr

	for {
		// Acquire a direction-1 credit before reading, if flow control is
		// active. Blocking here is safe (dedicated goroutine) and applies
		// TCP backpressure to the end-user when the destination is slow.
		if state.sendActive.Load() {
			select {
			case <-state.sendCh:
				// credit acquired
			case <-state.done:
				return // tunnel closed (e.g. PeerTunnelClose received)
			case <-peerDone:
				return // peer WebSocket connection died
			case <-p.manager.Done():
				return // manager shutting down
			}
		}

		n, err := pausableConn.Read(buf)
		if err != nil {
			// Connection closed or error from client side.
			break
		}

		messageSize := n + 1 + protocol.ClientIDLength // data + header

		// Bandwidth check with retry loop BEFORE sending to peer (counted at origin)
		if bandwidthScheduler != nil {
			// For tunneled traffic, we use a synthetic backend ID based on hostname
			// This ensures fair bandwidth distribution per hostname pool
			backendID := "tunnel:" + hostname
			for {
				allowed, waitTime := bandwidthScheduler.RequestSend(backendID, messageSize)
				if allowed {
					break
				}
				// Wait with shutdown handling
				select {
				case <-p.manager.Done():
					return // Manager shutting down
				case <-time.After(waitTime):
					// Continue to retry
				}
			}
		}

		header := make([]byte, 1+protocol.ClientIDLength)
		header[0] = protocol.PeerTunnelData
		copy(header[1:], clientID[:])
		message := append(header, buf[:n]...)
		sent := p.Send(message)

		if !sent {
			// Data dropped — refund the direction-1 credit so repeated
			// drops don't ratchet available credits down to zero.
			if state.sendActive.Load() {
				select {
				case state.sendCh <- struct{}{}:
				default:
				}
			}
		}

		if bandwidthScheduler != nil {
			backendID := "tunnel:" + hostname
			if sent {
				bandwidthScheduler.RecordSent(backendID, messageSize)
			} else {
				// Refund reserved bandwidth since the message was dropped
				bandwidthScheduler.RefundSend(backendID, messageSize)
			}
		}
	}

	// 3. Send a close message when the client disconnects.
	closeHeader := make([]byte, 1+protocol.ClientIDLength)
	closeHeader[0] = protocol.PeerTunnelClose
	copy(closeHeader[1:], clientID[:])
	p.Send(closeHeader)
}
