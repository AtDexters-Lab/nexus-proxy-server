package stun

import (
	"errors"
	"log"
	"net"
	"strconv"
	"sync"

	"github.com/pion/stun/v3"
)

// Server is an embedded STUN Binding server that responds to
// RFC 5389 Binding Requests with the client's reflexive transport address.
type Server struct {
	listenAddr string
	conn       net.PacketConn
	stopped    bool
	mu         sync.Mutex
	done       chan struct{}
}

// New creates a STUN server that will listen on the given UDP port.
func New(port int) *Server {
	return &Server{
		listenAddr: ":" + strconv.Itoa(port),
		done:       make(chan struct{}),
	}
}

// Run starts the STUN server. It blocks until Stop is called.
func (s *Server) Run() {
	defer close(s.done)

	pc, err := net.ListenPacket("udp", s.listenAddr)
	if err != nil {
		log.Fatalf("FATAL: [STUN] Failed to listen on %s: %v", s.listenAddr, err)
	}

	// Check if Stop was called while we were binding.
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		_ = pc.Close()
		return
	}
	s.conn = pc
	s.mu.Unlock()

	log.Printf("INFO: [STUN] Listening on %s", pc.LocalAddr())

	// Pre-allocate messages to avoid per-packet heap allocations.
	// Safe because the read loop is single-goroutine.
	var req stun.Message
	var resp stun.Message
	buf := make([]byte, 1500)

	for {
		n, addr, err := pc.ReadFrom(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				log.Println("INFO: [STUN] Server stopped")
				return
			}
			log.Printf("WARN: [STUN] ReadFrom error: %v", err)
			continue
		}
		s.handlePacket(pc, &req, &resp, buf[:n], addr)
	}
}

// Stop signals the server to shut down. If the server has already bound its
// socket, Stop closes it and waits for the read loop to exit. If Stop races
// with Run (socket not yet bound), it sets a flag that Run checks after binding.
// Safe to call even if Run was never called.
func (s *Server) Stop() {
	s.mu.Lock()
	s.stopped = true
	if s.conn == nil {
		s.mu.Unlock()
		return
	}
	_ = s.conn.Close()
	s.mu.Unlock()
	<-s.done
}

// LocalAddr returns the server's bound address, or nil if not yet listening.
func (s *Server) LocalAddr() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn == nil {
		return nil
	}
	return s.conn.LocalAddr()
}

func (s *Server) handlePacket(pc net.PacketConn, req, resp *stun.Message, data []byte, addr net.Addr) {
	if !stun.IsMessage(data) {
		return
	}

	if err := req.UnmarshalBinary(data); err != nil {
		return
	}

	if req.Type != stun.BindingRequest {
		return
	}

	udpAddr, ok := addr.(*net.UDPAddr)
	if !ok {
		return
	}

	if err := resp.Build(
		stun.NewTransactionIDSetter(req.TransactionID),
		stun.NewType(stun.MethodBinding, stun.ClassSuccessResponse),
		&stun.XORMappedAddress{
			IP:   udpAddr.IP,
			Port: udpAddr.Port,
		},
		stun.Fingerprint,
	); err != nil {
		log.Printf("WARN: [STUN] Failed to build response for %s: %v", addr, err)
		return
	}

	if _, err := pc.WriteTo(resp.Raw, addr); err != nil {
		log.Printf("WARN: [STUN] WriteTo %s failed: %v", addr, err)
	}
}
