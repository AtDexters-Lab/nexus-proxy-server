package proxy

import (
	"container/list"
	"errors"
	"io"
	"log"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
)

type udpClientConn struct {
	pc          net.PacketConn
	local       *net.UDPAddr
	remote      *net.UDPAddr
	maxDatagram int
	closeOnce   sync.Once
	closed      chan struct{}
}

func newUDPClientConn(pc net.PacketConn, localPort int, remote *net.UDPAddr, maxDatagram int) *udpClientConn {
	// Copy remote addr so we don't retain a pointer that might be reused upstream.
	remoteCopy := *remote
	return &udpClientConn{
		pc:          pc,
		local:       &net.UDPAddr{Port: localPort},
		remote:      &remoteCopy,
		maxDatagram: maxDatagram,
		closed:      make(chan struct{}),
	}
}

func (c *udpClientConn) Read([]byte) (int, error) { return 0, io.EOF }

func (c *udpClientConn) Write(p []byte) (int, error) {
	select {
	case <-c.closed:
		return 0, net.ErrClosed
	default:
	}
	if c.maxDatagram > 0 && len(p) > c.maxDatagram {
		return 0, errors.New("udp datagram exceeds configured limit")
	}
	n, err := c.pc.WriteTo(p, c.remote)
	return n, err
}

func (c *udpClientConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return nil
}

func (c *udpClientConn) LocalAddr() net.Addr  { return c.local }
func (c *udpClientConn) RemoteAddr() net.Addr { return c.remote }

func (c *udpClientConn) SetDeadline(time.Time) error      { return nil }
func (c *udpClientConn) SetReadDeadline(time.Time) error  { return nil }
func (c *udpClientConn) SetWriteDeadline(time.Time) error { return nil }

type udpFlow struct {
	key     string
	backend interface {
		AddClient(net.Conn, uuid.UUID, string, bool) error
		RemoveClient(uuid.UUID)
		SendData(uuid.UUID, []byte) error
		ID() string
	}
	clientID uuid.UUID
	conn     net.Conn
	lastSeen time.Time
}

type udpFlowTable struct {
	mu          sync.Mutex
	flows       map[string]*list.Element
	lru         *list.List
	idleTimeout time.Duration
	maxFlows    int
}

func newUDPFlowTable(idleTimeout time.Duration, maxFlows int) *udpFlowTable {
	if maxFlows <= 0 {
		maxFlows = 200_000
	}
	return &udpFlowTable{
		flows:       make(map[string]*list.Element, 1024),
		lru:         list.New(),
		idleTimeout: idleTimeout,
		maxFlows:    maxFlows,
	}
}

func (t *udpFlowTable) setIdleTimeout(idleTimeout time.Duration) {
	t.mu.Lock()
	t.idleTimeout = idleTimeout
	t.mu.Unlock()
}

func (t *udpFlowTable) get(key string) (*udpFlow, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	el, ok := t.flows[key]
	if !ok {
		return nil, false
	}
	f := el.Value.(*udpFlow)
	f.lastSeen = time.Now()
	t.lru.MoveToFront(el)
	return f, true
}

func (t *udpFlowTable) remove(key string) (*udpFlow, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	el, ok := t.flows[key]
	if !ok {
		return nil, false
	}
	f := el.Value.(*udpFlow)
	t.lru.Remove(el)
	delete(t.flows, key)
	return f, true
}

func (t *udpFlowTable) add(flow *udpFlow) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if el, ok := t.flows[flow.key]; ok {
		// Replace existing entry (should be rare).
		t.lru.Remove(el)
		delete(t.flows, flow.key)
	}

	flow.lastSeen = time.Now()
	el := t.lru.PushFront(flow)
	t.flows[flow.key] = el

	t.evictLocked()
}

func (t *udpFlowTable) evict() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.evictLocked()
}

func (t *udpFlowTable) evictLocked() {
	now := time.Now()

	// Expire idle flows (idle timeout is per-port, so LRU tail implies oldest).
	for {
		back := t.lru.Back()
		if back == nil {
			break
		}
		f := back.Value.(*udpFlow)
		if t.idleTimeout > 0 && now.Sub(f.lastSeen) <= t.idleTimeout {
			break
		}
		t.lru.Remove(back)
		delete(t.flows, f.key)
		f.backend.RemoveClient(f.clientID)
		if f.conn != nil {
			_ = f.conn.Close()
		}
	}

	// Enforce capacity.
	for t.lru.Len() > t.maxFlows {
		back := t.lru.Back()
		if back == nil {
			break
		}
		f := back.Value.(*udpFlow)
		t.lru.Remove(back)
		delete(t.flows, f.key)
		f.backend.RemoveClient(f.clientID)
		if f.conn != nil {
			_ = f.conn.Close()
		}
	}
}

func (l *Listener) listenOnUDPPort(port int) {
	defer l.wg.Done()

	listenAddr := ":" + strconv.Itoa(port)
	pc, err := net.ListenPacket("udp", listenAddr)
	if err != nil {
		log.Fatalf("ERROR: Failed to start UDP listener on port %d: %v", port, err)
		return
	}
	l.mu.Lock()
	l.udpConns = append(l.udpConns, pc)
	l.mu.Unlock()

	log.Printf("INFO: Public UDP listener started on %s", listenAddr)

	routeKey := "udp:" + strconv.Itoa(port)
	maxDatagram := l.config.UDPMaxDatagramBytesOrDefault()
	if maxDatagram > 0 && maxDatagram < 65507 {
		// Detect oversize packets by reading max+1 bytes.
		maxDatagram = maxDatagram + 1
	}
	if maxDatagram <= 0 {
		maxDatagram = 2048 + 1
	}

	idleTimeout := l.config.UDPFlowIdleTimeoutDefault()
	if d, ok := l.hub.UDPFlowIdleTimeout(port); ok && d > 0 {
		idleTimeout = d
	}
	table := newUDPFlowTable(idleTimeout, l.config.UDPMaxFlowsOrDefault())

	// Background reaper: refresh idle timeout and evict expired flows periodically.
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				next := l.config.UDPFlowIdleTimeoutDefault()
				if d, ok := l.hub.UDPFlowIdleTimeout(port); ok && d > 0 {
					next = d
				}
				if next != idleTimeout {
					log.Printf("INFO: Updating UDP flow idle timeout for port %d: %s -> %s", port, idleTimeout, next)
					idleTimeout = next
					table.setIdleTimeout(next)
				}
				table.evict()
			}
		}
	}()

	buf := make([]byte, maxDatagram)
	for {
		n, addr, err := pc.ReadFrom(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				close(done)
				return
			}
			log.Printf("ERROR: Failed to read UDP packet on %s: %v", listenAddr, err)
			continue
		}
		udpAddr, ok := addr.(*net.UDPAddr)
		if !ok {
			continue
		}
		if l.config.UDPMaxDatagramBytesOrDefault() > 0 && n > l.config.UDPMaxDatagramBytesOrDefault() {
			log.Printf("WARN: Dropping oversized UDP datagram (%d bytes) from %s on %s", n, udpAddr, listenAddr)
			continue
		}

		key := udpAddr.String()
		if flow, ok := table.get(key); ok {
			if err := flow.backend.SendData(flow.clientID, buf[:n]); err != nil {
				log.Printf("WARN: Failed to forward UDP datagram to backend %s for %s on %s: %v", flow.backend.ID(), udpAddr, listenAddr, err)
				_, _ = table.remove(key)
				flow.backend.RemoveClient(flow.clientID)
				if flow.conn != nil {
					_ = flow.conn.Close()
				}
			}
			continue
		}

		// Attempt local backend selection first.
		backend, err := l.hub.SelectBackend(routeKey)
		if err != nil {
			// No local backend; attempt peer forwarding if configured.
			if l.peerManager != nil {
				_ = l.peerManager.ForwardUDP(routeKey, port, pc, udpAddr, buf[:n])
			}
			continue
		}

		clientID := uuid.New()
		clientConn := newUDPClientConn(pc, port, udpAddr, l.config.UDPMaxDatagramBytesOrDefault())
		if err := backend.AddClient(clientConn, clientID, routeKey, false); err != nil {
			log.Printf("WARN: Failed to register UDP flow for %s on %s: %v", udpAddr, listenAddr, err)
			continue
		}

		flow := &udpFlow{
			key:      key,
			backend:  backend,
			clientID: clientID,
			conn:     clientConn,
		}
		table.add(flow)

		if err := backend.SendData(clientID, buf[:n]); err != nil {
			log.Printf("WARN: Failed to forward first UDP datagram to backend %s for %s on %s: %v", backend.ID(), udpAddr, listenAddr, err)
			_, _ = table.remove(key)
			backend.RemoveClient(clientID)
			_ = clientConn.Close()
			continue
		}
	}
}
