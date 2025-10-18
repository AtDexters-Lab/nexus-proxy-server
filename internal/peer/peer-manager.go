package peer

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"log"
	"sync"
	"sync/atomic"

	"github.com/AtDexters-Lab/nexus-proxy-server/internal/config"
	"github.com/AtDexters-Lab/nexus-proxy-server/internal/iface"
	"github.com/AtDexters-Lab/nexus-proxy-server/internal/protocol"
	"github.com/AtDexters-Lab/nexus-proxy-server/internal/proxy"
	"github.com/AtDexters-Lab/nexus-proxy-server/internal/routing"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// Manager is responsible for establishing and maintaining connections
// to all peer Nexus nodes and managing the global routing table.
type Manager struct {
	config        *config.Config
	hub           iface.Hub
	routingTable  *routing.Table
	outboundPeers sync.Map // Map of peer address to *Peer
	inboundPeers  sync.Map // Map of peer connection to *Peer
	tunnels       sync.Map // Map of original client ID to its tunnel
	localVersion  atomic.Uint64
	ctx           context.Context
	cancel        context.CancelFunc
	tlsBase       *tls.Config
}

// NewManager creates a new peer manager.
func NewManager(cfg *config.Config, hub iface.Hub, tlsBase *tls.Config) *Manager {
	return &Manager{
		config:       cfg,
		hub:          hub,
		routingTable: routing.NewTable(),
		tlsBase:      tlsBase,
	}
}

// Run starts the manager, which will attempt to connect to all configured peers.
func (m *Manager) Run(ctx context.Context) {
	m.ctx, m.cancel = context.WithCancel(ctx)

	if len(m.config.Peers) == 0 {
		log.Println("INFO: No peers configured, running in standalone mode.")
		return
	}
	log.Printf("INFO: Peer Manager starting, attempting to connect to %d peer(s)...", len(m.config.Peers))

	var wg sync.WaitGroup
	for _, addr := range m.config.Peers {
		wg.Add(1)
		go func(peerAddr string) {
			defer wg.Done()
			peer := NewPeer(peerAddr, m.config, m)
			m.outboundPeers.Store(peerAddr, peer)
			peer.Connect(m.ctx)
			m.outboundPeers.Delete(peerAddr)
		}(addr)
	}

	wg.Wait()
	log.Println("INFO: Peer Manager has stopped.")
}

// Stop signals all peer connections to terminate.
func (m *Manager) Stop() {
	log.Println("INFO: Stopping Peer Manager...")
	if m.cancel != nil {
		m.cancel()
	}
}

// HandleInboundPeer manages a new connection initiated by another peer.
func (m *Manager) HandleInboundPeer(conn *websocket.Conn) {
	peer := NewPeer(conn.RemoteAddr().String(), m.config, m)
	peer.conn = conn
	m.inboundPeers.Store(conn, peer)
	defer m.inboundPeers.Delete(conn)

	peer.handleConnection(m.ctx)
}

// AnnounceLocalRoutes calculates the current local routes, increments the state
// version, and broadcasts the new state to all connected peers.
func (m *Manager) AnnounceLocalRoutes() {
	newVersion := m.localVersion.Add(1)
	hostnames := m.hub.GetLocalRoutes()

	msg := protocol.PeerMessage{
		Version:   newVersion,
		Type:      protocol.PeerAnnounce,
		Hostnames: hostnames,
	}

	payload, err := json.Marshal(msg)
	if err != nil {
		log.Printf("ERROR: Failed to marshal peer announcement: %v", err)
		return
	}

	log.Printf("INFO: Announcing local state version %d with %d routes to peers.", newVersion, len(hostnames))
	m.broadcastToPeers(payload)
}

// UpdatePeerRoutes is called by a Peer when it receives an announcement.
func (m *Manager) UpdatePeerRoutes(p *peerImpl, version uint64, hostnames []string) {
	m.routingTable.UpdateRoutesForPeer(p, version, hostnames)
}

// ClearRoutesForPeer is called when a peer disconnects, to purge its routes.
func (m *Manager) ClearRoutesForPeer(p *peerImpl) {
	m.routingTable.ClearRoutesForPeer(p)
	log.Printf("INFO: Cleared all routes for disconnected peer %s", p.addr)
}

// GetPeerForHostname checks the routing table for a peer that can service a given hostname.
func (m *Manager) GetPeerForHostname(hostname string) (iface.Peer, bool) {
	return m.routingTable.GetPeerForHostname(hostname)
}

// HandleTunnelRequest is called by a peer's read pump when it receives a request to
// establish a tunnel for a client. It selects a local backend and starts the proxying.
func (m *Manager) HandleTunnelRequest(p iface.Peer, hostname string, clientID uuid.UUID, clientIP string, connPort int, isTLS bool) {
	log.Printf("INFO: [TUNNEL-IN] Received tunnel request for client %s to hostname '%s' from peer %s (TLS: %v)", clientID, hostname, p.Addr(), isTLS)
	backend, err := m.hub.SelectBackend(hostname)
	if err != nil {
		log.Printf("WARN: [TUNNEL-IN] No local backend for tunneled client %s. Ignoring.", clientID)
		// TODO: Send a "tunnel_failed" message back to the originating peer.
		return
	}

	// Create a virtual client connection that will be managed by the peer.
	tunneledConn := NewTunneledConn(clientID, p, clientIP, connPort)
	m.tunnels.Store(clientID, tunneledConn)

	// This looks like a regular client connection to the backend.
	// We pass a nil config because idle timeouts for tunneled connections are
	// managed by the originating proxy.
	client := proxy.NewClient(tunneledConn, backend, nil, hostname, isTLS)
	go client.Start() // Run in a goroutine because this is initiated by the peer's read pump.
}

// HandleTunnelData forwards data received from a peer to the correct tunneled connection.
func (m *Manager) HandleTunnelData(clientID uuid.UUID, payload []byte) {
	if rawTunnel, ok := m.tunnels.Load(clientID); ok {
		if tunnel, ok := rawTunnel.(*TunneledConn); ok {
			// This is the corrected logic: write the data into the pipe
			// so the local backend's proxy loop can read it.
			if _, err := tunnel.WriteToPipe(payload); err != nil {
				log.Printf("WARN: Failed to write to tunnel pipe for client %s: %v", clientID, err)
				tunnel.Close()
			}
		}
	}
}

// HandleTunnelClose cleans up a tunnel when a peer signals it has closed.
func (m *Manager) HandleTunnelClose(clientID uuid.UUID) {
	if rawTunnel, ok := m.tunnels.Load(clientID); ok {
		if tunnel, ok := rawTunnel.(*TunneledConn); ok {
			tunnel.Close()
			m.tunnels.Delete(clientID)
		}
	}
}

// broadcastToPeers sends a message to all connected outbound and inbound peers.
func (m *Manager) broadcastToPeers(payload []byte) {
	m.outboundPeers.Range(func(key, value interface{}) bool {
		if p, ok := value.(*peerImpl); ok {
			p.Send(payload)
		}
		return true
	})
	m.inboundPeers.Range(func(key, value interface{}) bool {
		if p, ok := value.(*peerImpl); ok {
			p.Send(payload)
		}
		return true
	})
}
