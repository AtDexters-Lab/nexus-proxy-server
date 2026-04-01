package peer

import (
	"container/list"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"log"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AtDexters-Lab/nexus-proxy/internal/bandwidth"
	"github.com/AtDexters-Lab/nexus-proxy/internal/config"
	"github.com/AtDexters-Lab/nexus-proxy/internal/iface"
	"github.com/AtDexters-Lab/nexus-proxy/protocol"
	"github.com/AtDexters-Lab/nexus-proxy/internal/proxy"
	"github.com/AtDexters-Lab/nexus-proxy/internal/routing"
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
	udpOutbound  *udpOutboundFlowTable
	localVersion atomic.Uint64
	ctx           context.Context
	cancel        context.CancelFunc
	tlsBase       *tls.Config
}

// udpOutboundFlowTable is a thread-safe LRU cache for outbound UDP flows.
// It provides O(1) lookups by key and ID, and O(1) eviction of the oldest flow.
type udpOutboundFlowTable struct {
	mu       sync.Mutex
	byKey    map[udpOutboundKey]*list.Element
	byID     map[uuid.UUID]*list.Element
	lru      *list.List
	maxFlows int
}

func newUDPOutboundFlowTable(maxFlows int) *udpOutboundFlowTable {
	if maxFlows <= 0 {
		maxFlows = 200_000
	}
	return &udpOutboundFlowTable{
		byKey:    make(map[udpOutboundKey]*list.Element, 1024),
		byID:     make(map[uuid.UUID]*list.Element, 1024),
		lru:      list.New(),
		maxFlows: maxFlows,
	}
}

func (t *udpOutboundFlowTable) getByKey(key udpOutboundKey) (*udpOutboundFlow, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	el, ok := t.byKey[key]
	if !ok {
		return nil, false
	}
	flow := el.Value.(*udpOutboundFlow)
	flow.touch()
	t.lru.MoveToFront(el)
	return flow, true
}

func (t *udpOutboundFlowTable) getByID(id uuid.UUID) (*udpOutboundFlow, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	el, ok := t.byID[id]
	if !ok {
		return nil, false
	}
	flow := el.Value.(*udpOutboundFlow)
	flow.touch()
	t.lru.MoveToFront(el)
	return flow, true
}

// peekByID returns a flow without touching it (for reaper inspection).
func (t *udpOutboundFlowTable) peekByID(id uuid.UUID) (*udpOutboundFlow, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	el, ok := t.byID[id]
	if !ok {
		return nil, false
	}
	return el.Value.(*udpOutboundFlow), true
}

func (t *udpOutboundFlowTable) add(flow *udpOutboundFlow) {
	t.mu.Lock()
	defer t.mu.Unlock()

	// Remove existing entry if present (shouldn't happen normally).
	if el, ok := t.byKey[flow.key]; ok {
		t.removeLocked(el.Value.(*udpOutboundFlow))
	}

	flow.touch()
	el := t.lru.PushFront(flow)
	t.byKey[flow.key] = el
	t.byID[flow.id] = el
}

// addIfAbsent atomically adds a flow only if no flow with the same key exists.
// Returns (existing flow, false) if a flow already exists, or (nil, true) if added.
func (t *udpOutboundFlowTable) addIfAbsent(flow *udpOutboundFlow) (*udpOutboundFlow, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if el, ok := t.byKey[flow.key]; ok {
		existing := el.Value.(*udpOutboundFlow)
		existing.touch()
		t.lru.MoveToFront(el)
		return existing, false
	}

	flow.touch()
	el := t.lru.PushFront(flow)
	t.byKey[flow.key] = el
	t.byID[flow.id] = el
	return nil, true
}

func (t *udpOutboundFlowTable) remove(flow *udpOutboundFlow) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.removeLocked(flow)
}

func (t *udpOutboundFlowTable) removeLocked(flow *udpOutboundFlow) bool {
	el, ok := t.byID[flow.id]
	if !ok {
		return false
	}
	t.lru.Remove(el)
	delete(t.byKey, flow.key)
	delete(t.byID, flow.id)
	return true
}

func (t *udpOutboundFlowTable) evictOldest() *udpOutboundFlow {
	t.mu.Lock()
	defer t.mu.Unlock()
	back := t.lru.Back()
	if back == nil {
		return nil
	}
	flow := back.Value.(*udpOutboundFlow)
	t.removeLocked(flow)
	return flow
}

func (t *udpOutboundFlowTable) count() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.lru.Len()
}

func (t *udpOutboundFlowTable) isFull() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.lru.Len() >= t.maxFlows
}

// reapExpired removes all flows that have exceeded their idle timeout.
// Returns the list of evicted flows (caller must handle cleanup like sendClose).
func (t *udpOutboundFlowTable) reapExpired(now time.Time) []*udpOutboundFlow {
	t.mu.Lock()
	defer t.mu.Unlock()

	var evicted []*udpOutboundFlow
	for {
		back := t.lru.Back()
		if back == nil {
			break
		}
		flow := back.Value.(*udpOutboundFlow)
		if flow.idleTimeout <= 0 {
			break
		}
		last := flow.lastSeen()
		if last.IsZero() || now.Sub(last) <= flow.idleTimeout {
			break
		}
		t.removeLocked(flow)
		evicted = append(evicted, flow)
	}
	return evicted
}

type udpOutboundKey struct {
	client  string
	dstPort int
}

type udpOutboundFlow struct {
	id            uuid.UUID
	key           udpOutboundKey
	peer          iface.Peer
	pc            net.PacketConn
	clientAddr    *net.UDPAddr
	routeKey      string
	bandwidthID   string // synthetic ID for bandwidth accounting ("tunnel:<routeKey>")
	lastSeenNanos atomic.Int64
	idleTimeout   time.Duration
}

func (f *udpOutboundFlow) touch() {
	f.lastSeenNanos.Store(time.Now().UnixNano())
}

func (f *udpOutboundFlow) lastSeen() time.Time {
	n := f.lastSeenNanos.Load()
	if n <= 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

func (f *udpOutboundFlow) sendOpen() error {
	msg := protocol.PeerMessage{
		Type:      protocol.PeerTunnelRequest,
		Hostname:  f.routeKey,
		ClientID:  f.id,
		ConnPort:  f.key.dstPort,
		ClientIP:  f.key.client,
		Transport: protocol.TransportUDP,
		IsTLS:     false,
	}
	payload, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	if ok := f.peer.Send(payload); !ok {
		return errors.New("peer send queue full")
	}
	return nil
}

func (f *udpOutboundFlow) sendDatagram(payload []byte) error {
	header := make([]byte, 1+protocol.ClientIDLength)
	header[0] = protocol.PeerTunnelData
	copy(header[1:], f.id[:])
	message := append(header, payload...)
	if ok := f.peer.Send(message); !ok {
		return errors.New("peer send queue full")
	}
	return nil
}

func (f *udpOutboundFlow) sendClose() {
	header := make([]byte, 1+protocol.ClientIDLength)
	header[0] = protocol.PeerTunnelClose
	copy(header[1:], f.id[:])
	f.peer.Send(header)
}

// NewManager creates a new peer manager.
func NewManager(cfg *config.Config, hub iface.Hub, tlsBase *tls.Config) *Manager {
	return &Manager{
		config:       cfg,
		hub:          hub,
		routingTable: routing.NewTable(),
		udpOutbound:  newUDPOutboundFlowTable(cfg.UDPMaxFlowsOrDefault()),
		tlsBase:      tlsBase,
	}
}

// Run starts the manager, which will attempt to connect to all configured peers.
func (m *Manager) Run(ctx context.Context) {
	m.ctx, m.cancel = context.WithCancel(ctx)
	go m.udpOutboundReaper()

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

func (m *Manager) udpOutboundReaper() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-m.Done():
			return
		case <-ticker.C:
		}

		evicted := m.udpOutbound.reapExpired(time.Now())
		for _, flow := range evicted {
			m.cleanupOutboundUDPFlow(flow, true)
		}
	}
}

// cleanupOutboundUDPFlow handles bandwidth unregistration and optional close message.
// The flow must already be removed from the table before calling this.
func (m *Manager) cleanupOutboundUDPFlow(flow *udpOutboundFlow, sendClose bool) {
	if flow == nil {
		return
	}
	if flow.bandwidthID != "" {
		if sched := m.GetBandwidthScheduler(); sched != nil {
			sched.UnregisterShared(flow.bandwidthID)
		}
	}
	if sendClose {
		flow.sendClose()
	}
}

func (m *Manager) HandleOutboundUDPData(flowID uuid.UUID, payload []byte) bool {
	flow, ok := m.udpOutbound.getByID(flowID)
	if !ok {
		return false
	}

	maxDatagram := m.config.UDPMaxDatagramBytesOrDefault()
	if maxDatagram > 0 && len(payload) > maxDatagram {
		log.Printf("WARN: Dropping oversized UDP tunnel datagram (%d bytes) for flow %s (%s)", len(payload), flowID, flow.routeKey)
		return true
	}

	if _, err := flow.pc.WriteTo(payload, flow.clientAddr); err != nil {
		log.Printf("WARN: Failed to write UDP tunnel response to %s for flow %s (%s): %v", flow.clientAddr, flowID, flow.routeKey, err)
		if m.udpOutbound.remove(flow) {
			m.cleanupOutboundUDPFlow(flow, true)
		}
	}
	return true
}

func (m *Manager) HandleOutboundUDPClose(flowID uuid.UUID) bool {
	flow, ok := m.udpOutbound.peekByID(flowID)
	if !ok {
		return false
	}
	if m.udpOutbound.remove(flow) {
		m.cleanupOutboundUDPFlow(flow, false)
	}
	return true
}

func (m *Manager) ForwardUDP(routeKey string, dstPort int, pc net.PacketConn, clientAddr *net.UDPAddr, payload []byte) error {
	if pc == nil {
		return errors.New("packet conn is nil")
	}
	if clientAddr == nil {
		return errors.New("client addr is nil")
	}
	maxDatagram := m.config.UDPMaxDatagramBytesOrDefault()
	if maxDatagram > 0 && len(payload) > maxDatagram {
		return errors.New("udp datagram exceeds configured limit")
	}

	// Message size for bandwidth accounting: header + clientID + payload
	messageSize := 1 + protocol.ClientIDLength + len(payload)
	bandwidthScheduler := m.GetBandwidthScheduler()

	key := udpOutboundKey{client: clientAddr.String(), dstPort: dstPort}

	// Fast path: try existing flow.
	if flow, ok := m.udpOutbound.getByKey(key); ok {
		if bandwidthScheduler != nil && flow.bandwidthID != "" {
			if allowed, _ := bandwidthScheduler.RequestSend(flow.bandwidthID, messageSize); !allowed {
				return nil // Drop datagram if bandwidth exhausted (UDP is best-effort)
			}
		}
		if err := flow.sendDatagram(payload); err == nil {
			if bandwidthScheduler != nil && flow.bandwidthID != "" {
				bandwidthScheduler.RecordSent(flow.bandwidthID, messageSize)
			}
			return nil
		}
		if bandwidthScheduler != nil && flow.bandwidthID != "" {
			bandwidthScheduler.RefundSend(flow.bandwidthID, messageSize)
		}
		if m.udpOutbound.remove(flow) {
			m.cleanupOutboundUDPFlow(flow, true)
		}
	}

	// Slow path: create new flow.
	peer, ok := m.GetPeerForHostname(routeKey)
	if !ok {
		return errors.New("no peer available for route")
	}

	// Evict oldest flow if at capacity.
	if m.udpOutbound.isFull() {
		if evicted := m.udpOutbound.evictOldest(); evicted != nil {
			m.cleanupOutboundUDPFlow(evicted, true)
		}
	}

	addrCopy := *clientAddr
	idleTimeout := m.config.UDPFlowIdleTimeoutDefault()
	if d, ok := m.hub.UDPFlowIdleTimeout(dstPort); ok && d > 0 {
		idleTimeout = d
	}

	bandwidthID := "tunnel:" + routeKey
	flow := &udpOutboundFlow{
		id:          uuid.New(),
		key:         key,
		peer:        peer,
		pc:          pc,
		clientAddr:  &addrCopy,
		routeKey:    routeKey,
		bandwidthID: bandwidthID,
		idleTimeout: idleTimeout,
	}

	// Atomically add the flow; if another goroutine raced us, use the existing flow.
	if existing, added := m.udpOutbound.addIfAbsent(flow); !added {
		// Use the existing flow that won the race.
		if bandwidthScheduler != nil && existing.bandwidthID != "" {
			if allowed, _ := bandwidthScheduler.RequestSend(existing.bandwidthID, messageSize); !allowed {
				return nil
			}
		}
		if err := existing.sendDatagram(payload); err == nil {
			if bandwidthScheduler != nil && existing.bandwidthID != "" {
				bandwidthScheduler.RecordSent(existing.bandwidthID, messageSize)
			}
			return nil
		}
		if bandwidthScheduler != nil && existing.bandwidthID != "" {
			bandwidthScheduler.RefundSend(existing.bandwidthID, messageSize)
		}
		if m.udpOutbound.remove(existing) {
			m.cleanupOutboundUDPFlow(existing, true)
		}
		return errors.New("flow send failed")
	}

	// We added a new flow; register with bandwidth scheduler.
	if bandwidthScheduler != nil {
		bandwidthScheduler.RegisterShared(bandwidthID)
	}

	if err := flow.sendOpen(); err != nil {
		if m.udpOutbound.remove(flow) {
			m.cleanupOutboundUDPFlow(flow, false)
		}
		return err
	}

	// Bandwidth check before sending first datagram.
	if bandwidthScheduler != nil {
		if allowed, _ := bandwidthScheduler.RequestSend(bandwidthID, messageSize); !allowed {
			return nil // Flow created but first datagram dropped due to bandwidth
		}
	}
	if err := flow.sendDatagram(payload); err != nil {
		if bandwidthScheduler != nil {
			bandwidthScheduler.RefundSend(bandwidthID, messageSize)
		}
		if m.udpOutbound.remove(flow) {
			m.cleanupOutboundUDPFlow(flow, true)
		}
		return err
	}
	if bandwidthScheduler != nil {
		bandwidthScheduler.RecordSent(bandwidthID, messageSize)
	}
	return nil
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
	if strings.HasPrefix(hostname, protocol.RouteKeyPrefixUDP) {
		log.Printf("INFO: [UDP-TUNNEL-IN] Received tunnel request for client %s to route '%s' from peer %s", clientID, hostname, p.Addr())
		backend, err := m.hub.SelectBackend(hostname)
		if err != nil {
			log.Printf("WARN: [UDP-TUNNEL-IN] No local backend for tunneled client %s. Ignoring.", clientID)
			return
		}

		remoteAddr, err := net.ResolveUDPAddr("udp", clientIP)
		if err != nil {
			log.Printf("WARN: [UDP-TUNNEL-IN] Could not parse client address '%s' for client %s: %v", clientIP, clientID, err)
			return
		}

		conn := newTunneledUDPConn(clientID, p, remoteAddr, connPort, m.config.UDPMaxDatagramBytesOrDefault())
		conn.onClose = func() {
			if _, loaded := m.tunnels.LoadAndDelete(clientID); loaded {
				backend.RemoveClient(clientID)
			}
		}
		if err := backend.AddClient(conn, clientID, hostname, false); err != nil {
			log.Printf("WARN: [UDP-TUNNEL-IN] Failed to register client %s with backend %s for route '%s': %v", clientID, backend.ID(), hostname, err)
			conn.onClose = nil // prevent callback from removing a client that was never added
			_ = conn.Close()
			return
		}
		m.tunnels.Store(clientID, &udpInboundTunnel{backend: backend, conn: conn})
		return
	}

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
	go func() {
		// Ensure the tunnel entry is always cleared, even if the backend rejects
		// the client immediately or the connection closes normally.
		defer m.tunnels.Delete(clientID)
		client.Start()
	}()
}

// HandleTunnelData forwards data received from a peer to the correct tunneled connection.
func (m *Manager) HandleTunnelData(clientID uuid.UUID, payload []byte) {
	if rawTunnel, ok := m.tunnels.Load(clientID); ok {
		switch tunnel := rawTunnel.(type) {
		case *TunneledConn:
			// This is the corrected logic: write the data into the pipe
			// so the local backend's proxy loop can read it.
			if _, err := tunnel.WriteToPipe(payload); err != nil {
				log.Printf("WARN: Failed to write to tunnel pipe for client %s: %v", clientID, err)
				tunnel.Close()
			}
		case *udpInboundTunnel:
			if max := m.config.UDPMaxDatagramBytesOrDefault(); max > 0 && len(payload) > max {
				log.Printf("WARN: Dropping oversized UDP tunnel datagram (%d bytes) for inbound flow %s", len(payload), clientID)
				return
			}
			if err := tunnel.backend.SendData(clientID, payload); err != nil {
				log.Printf("WARN: Failed to forward UDP tunnel datagram for inbound flow %s: %v", clientID, err)
				tunnel.backend.RemoveClient(clientID)
				if tunnel.conn != nil {
					_ = tunnel.conn.Close()
				}
				m.tunnels.Delete(clientID)
			}
		}
	}
}

// HandleTunnelClose cleans up a tunnel when a peer signals it has closed.
func (m *Manager) HandleTunnelClose(clientID uuid.UUID) {
	if rawTunnel, ok := m.tunnels.LoadAndDelete(clientID); ok {
		switch tunnel := rawTunnel.(type) {
		case *TunneledConn:
			tunnel.Close()
		case *udpInboundTunnel:
			tunnel.backend.RemoveClient(clientID)
			if tunnel.conn != nil {
				_ = tunnel.conn.Close()
			}
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

// GetBandwidthScheduler returns the bandwidth scheduler from the hub.
// Used by peers for bandwidth limiting during tunneling.
func (m *Manager) GetBandwidthScheduler() *bandwidth.Scheduler {
	return m.hub.GetBandwidthScheduler()
}

// Done returns a channel that is closed when the manager is shutting down.
// Used by peers for graceful shutdown during bandwidth waits.
func (m *Manager) Done() <-chan struct{} {
	if m.ctx == nil {
		// Return a never-closing channel if context not yet initialized
		return make(chan struct{})
	}
	return m.ctx.Done()
}
