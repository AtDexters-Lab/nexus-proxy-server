package hub

import (
	"context"
	crand "crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AtDexters-Lab/nexus-proxy-server/internal/auth"
	"github.com/AtDexters-Lab/nexus-proxy-server/internal/config"
	"github.com/AtDexters-Lab/nexus-proxy-server/internal/protocol"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

const (
	writeWait      = 10 * time.Second
	pongWait       = 60 * time.Second
	pingPeriod     = (pongWait * 9) / 10
	maxMessageSize = 32*1024 + protocol.MessageHeaderLength // This must be sent within writeWait

	defaultReauthGrace = 10 * time.Second
	healthCheckTimeout = 5 * time.Second
)

type outboundMessage struct {
	messageType int
	data        []byte
}

// Backend represents a single, authenticated WebSocket connection from a backend service.
type Backend struct {
	id        string
	conn      *websocket.Conn
	config    *config.Config
	validator auth.Validator

	hostnames     []string
	weight        int
	policyVersion string

	clients sync.Map

	outgoing     chan outboundMessage
	reauthTokens chan string

	quit      chan struct{}
	closeOnce sync.Once
	closed    atomic.Bool

	reauthInterval      time.Duration
	reauthGrace         time.Duration
	maintenanceCap      time.Duration
	maintenanceUsed     time.Duration
	authorizerStatusURI string

	reauthMu      sync.Mutex
	reauthPending bool
	pendingNonce  string

	httpClient *http.Client
}

// NewBackend creates a new Backend instance.
func NewBackend(conn *websocket.Conn, meta *AttestationMetadata, cfg *config.Config, validator auth.Validator) *Backend {
	maintenanceCap := meta.MaintenanceCap
	if !meta.HasMaintenanceCap {
		maintenanceCap = cfg.MaintenanceGraceDefault()
	}

	httpClient := &http.Client{Timeout: cfg.RemoteVerifierTimeout()}

	b := &Backend{
		id:                  uuid.New().String(),
		conn:                conn,
		config:              cfg,
		validator:           validator,
		hostnames:           meta.cloneHostnames(),
		weight:              meta.Weight,
		policyVersion:       meta.PolicyVersion,
		outgoing:            make(chan outboundMessage, 256),
		reauthTokens:        make(chan string, 1),
		quit:                make(chan struct{}),
		reauthInterval:      meta.ReauthInterval,
		reauthGrace:         meta.ReauthGrace,
		maintenanceCap:      maintenanceCap,
		authorizerStatusURI: meta.AuthorizerStatusURI,
		httpClient:          httpClient,
	}

	if b.reauthInterval > 0 && b.reauthGrace <= 0 {
		b.reauthGrace = defaultReauthGrace
	}

	return b
}

func (b *Backend) ID() string {
	return b.id
}

func (b *Backend) Close() {
	b.closeOnce.Do(func() {
		b.closed.Store(true)
		close(b.quit)
		if b.conn != nil {
			_ = b.conn.Close()
		}
		b.clients.Range(func(key, value interface{}) bool {
			if clientConn, ok := value.(net.Conn); ok {
				_ = clientConn.Close()
			}
			return true
		})
	})
}

func (b *Backend) StartPumps() {
	var wg sync.WaitGroup
	wg.Add(3)

	go func() {
		defer wg.Done()
		b.writePump()
	}()

	go func() {
		defer wg.Done()
		b.readPump()
	}()

	go func() {
		defer wg.Done()
		b.reauthLoop()
	}()

	wg.Wait()
}

func (b *Backend) AddClient(clientConn net.Conn, clientID uuid.UUID, hostname string, isTLS bool) error {
	var connPort int
	if tcpAddr, ok := clientConn.LocalAddr().(*net.TCPAddr); ok {
		connPort = tcpAddr.Port
	} else {
		return fmt.Errorf("WARN: Could not determine destination port for client %s", clientID)
	}
	msg := protocol.ControlMessage{
		Event:    protocol.EventConnect,
		ClientID: clientID,
		ConnPort: connPort,
		ClientIP: clientConn.RemoteAddr().String(),
		Hostname: hostname,
		IsTLS:    isTLS,
	}

	if err := b.SendControlMessage(msg); err != nil {
		return fmt.Errorf("failed to send connect message for client %s: %w", clientID, err)
	}
	b.clients.Store(clientID, clientConn)
	return nil
}

func (b *Backend) RemoveClient(clientID uuid.UUID) {
	if _, ok := b.clients.Load(clientID); ok {
		b.clients.Delete(clientID)
		msg := protocol.ControlMessage{Event: protocol.EventDisconnect, ClientID: clientID}
		if err := b.SendControlMessage(msg); err != nil {
			log.Printf("ERROR: Failed to send disconnect message for client %s: %v", clientID, err)
		} else {
			log.Printf("INFO: Client %s disconnected from backend %s", clientID, b.id)
		}
	}
}

func (b *Backend) SendData(clientID uuid.UUID, data []byte) error {
	if b.closed.Load() {
		return fmt.Errorf("backend %s is already closed", b.id)
	}
	header := make([]byte, 1+protocol.ClientIDLength)
	header[0] = protocol.ControlByteData
	copy(header[1:], clientID[:])
	message := append(header, data...)

	outbound := outboundMessage{messageType: websocket.BinaryMessage, data: message}
	select {
	case <-b.quit:
		return fmt.Errorf("backend %s is closing", b.id)
	case b.outgoing <- outbound:
	default:
		return fmt.Errorf("backend %s send channel full, dropping data for client %s", b.id, clientID)
	}
	select {
	case <-b.quit:
		return fmt.Errorf("backend %s is closing", b.id)
	default:
	}
	return nil
}

func (b *Backend) SendControlMessage(msg protocol.ControlMessage) error {
	if b.closed.Load() {
		return fmt.Errorf("backend %s is already closed", b.id)
	}

	payload, err := json.Marshal(msg)
	if err != nil {
		log.Printf("ERROR: Failed to marshal control message for backend %s: %v", b.id, err)
		return err
	}

	header := []byte{protocol.ControlByteControl}
	outbound := outboundMessage{messageType: websocket.BinaryMessage, data: append(header, payload...)}

	select {
	case <-b.quit:
		return fmt.Errorf("backend %s is closing", b.id)
	case b.outgoing <- outbound:
	default:
		return fmt.Errorf("backend %s send channel full, dropping data for client %s", b.id, msg.ClientID)
	}
	select {
	case <-b.quit:
		return fmt.Errorf("backend %s is closing", b.id)
	default:
	}
	return nil
}

func (b *Backend) readPump() {
	defer func() {
		log.Printf("INFO: Closing backend %s read pump", b.id)
		b.Close()
	}()
	b.conn.SetReadLimit(maxMessageSize)
	b.conn.SetReadDeadline(time.Now().Add(pongWait))
	b.conn.SetPongHandler(func(string) error {
		b.conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

	for {
		messageType, message, err := b.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("ERROR: Unexpected close error from backend %s: %v", b.id, err)
			}
			break
		}

		b.conn.SetReadDeadline(time.Now().Add(pongWait))

		switch messageType {
		case websocket.BinaryMessage:
			b.handleBinaryMessage(message)
		case websocket.TextMessage:
			b.handleTextMessage(string(message))
		default:
			log.Printf("WARN: Unsupported websocket message type %d from backend %s", messageType, b.id)
		}
	}
}

func (b *Backend) handleBinaryMessage(message []byte) {
	if len(message) < 1 {
		return
	}

	controlByte := message[0]
	payload := message[1:]

	switch controlByte {
	case protocol.ControlByteControl:
		var msg protocol.ControlMessage
		if err := json.Unmarshal(payload, &msg); err != nil {
			log.Printf("WARN: Malformed control message from backend %s", b.id)
			return
		}
		b.handleControlMessage(msg)

	case protocol.ControlByteData:
		if len(payload) < protocol.ClientIDLength {
			log.Printf("WARN: Malformed data message from backend %s", b.id)
			return
		}
		var clientID uuid.UUID
		copy(clientID[:], payload[:protocol.ClientIDLength])
		data := payload[protocol.ClientIDLength:]

		if rawConn, ok := b.clients.Load(clientID); ok {
			if clientConn, ok := rawConn.(net.Conn); ok {
				if b.config.IdleTimeout() > 0 {
					_ = clientConn.SetWriteDeadline(time.Now().Add(b.config.IdleTimeout()))
				}
				if _, err := clientConn.Write(data); err != nil {
					log.Printf("WARN: Failed to write to client %s: %v. Closing.", clientID, err)
					_ = clientConn.Close()
				}
			} else {
				log.Printf("ERROR: Client %s is not a net.Conn type in backend %s", clientID, b.id)
			}
		} else {
			log.Printf("WARN: Received data for unknown client %s from backend %s. Ignoring.", clientID, b.id)
		}
	}
}

func (b *Backend) handleTextMessage(message string) {
	b.reauthMu.Lock()
	expecting := b.reauthPending
	b.reauthMu.Unlock()

	if !expecting {
		log.Printf("WARN: Unexpected text frame from backend %s while no re-auth is pending", b.id)
		return
	}

	select {
	case b.reauthTokens <- message:
	default:
		log.Printf("WARN: Dropping re-auth token from backend %s: channel full", b.id)
	}
}

func (b *Backend) handleControlMessage(msg protocol.ControlMessage) {
	switch msg.Event {
	case protocol.EventPingClient:
		if _, ok := b.clients.Load(msg.ClientID); ok {
			pongMsg := protocol.ControlMessage{Event: protocol.EventPongClient, ClientID: msg.ClientID}
			if err := b.SendControlMessage(pongMsg); err != nil {
				log.Printf("ERROR: Failed to send pong for client %s to backend %s: %v", msg.ClientID, b.id, err)
			}
		} else {
			log.Printf("INFO: Client %s is no longer active, not sending pong to backend %s", msg.ClientID, b.id)
		}
	case protocol.EventDisconnect:
		if rawConn, ok := b.clients.Load(msg.ClientID); ok {
			if conn, ok := rawConn.(net.Conn); ok {
				log.Printf("INFO: Backend %s reported disconnect for client %s. Closing client connection.", b.id, msg.ClientID)
				b.clients.Delete(msg.ClientID)
				_ = conn.Close()
			}
		}
	default:
		log.Printf("WARN: Unknown control event '%s' from backend %s", msg.Event, b.id)
	}
}

func (b *Backend) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		log.Printf("INFO: Closing backend %s write pump", b.id)
		ticker.Stop()
		b.Close()
	}()
	for {
		select {
		case <-b.quit:
			return
		case outbound, ok := <-b.outgoing:
			_ = b.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				_ = b.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			if err := b.conn.WriteMessage(outbound.messageType, outbound.data); err != nil {
				log.Printf("ERROR: Failed to write to backend %s: %v", b.id, err)
				return
			}
		case <-ticker.C:
			_ = b.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := b.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

func (b *Backend) reauthLoop() {
	if b.reauthInterval <= 0 {
		<-b.quit
		return
	}
	timer := time.NewTimer(b.jitterDuration(b.reauthInterval))
	defer func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}()

	for {
		select {
		case <-b.quit:
			return
		case <-timer.C:
			if err := b.performReauth(); err != nil {
				log.Printf("ERROR: Re-authentication failed for backend %s: %v", b.id, err)
				b.Close()
				return
			}
			next := b.reauthInterval
			if next <= 0 {
				// Claims may disable future reauth.
				return
			}
			timer.Reset(b.jitterDuration(next))
		}
	}
}

func (b *Backend) performReauth() error {
	if b.authorizerStatusURI != "" {
		for {
			healthy, err := b.authorizerHealthy()
			if err == nil && healthy {
				b.maintenanceUsed = 0
				break
			}

			if !b.deferForMaintenance() {
				break
			}
		}
	}

	nonce, err := generateNonce()
	if err != nil {
		return fmt.Errorf("generate nonce: %w", err)
	}

	challenge := challengeMessage{Type: "reauth_challenge", Nonce: nonce}
	payload, err := json.Marshal(challenge)
	if err != nil {
		return fmt.Errorf("marshal reauth challenge: %w", err)
	}

	b.prepareForNonce(nonce)

	outbound := outboundMessage{messageType: websocket.TextMessage, data: payload}
	select {
	case <-b.quit:
		b.clearPendingNonce()
		return errors.New("backend closing during reauth challenge")
	case b.outgoing <- outbound:
	default:
		b.clearPendingNonce()
		return errors.New("reauth challenge channel full")
	}

	// Drain any stale token.
	select {
	case <-b.reauthTokens:
	default:
	}

	timer := time.NewTimer(b.reauthGrace)
	defer timer.Stop()

	var token string
	select {
	case <-b.quit:
		b.clearPendingNonce()
		return errors.New("backend closed during reauth")
	case <-timer.C:
		b.clearPendingNonce()
		return fmt.Errorf("reauthentication timed out after %s", b.reauthGrace)
	case token = <-b.reauthTokens:
	}
	b.clearPendingNonce()

	return b.processReauthToken(token, nonce)
}

func (b *Backend) processReauthToken(token, expectedNonce string) error {
	ctx, cancel := context.WithTimeout(context.Background(), authTimeout)
	defer cancel()

	claims, err := b.validator.Validate(ctx, token)
	if err != nil {
		return fmt.Errorf("validate token: %w", err)
	}
	if claims.SessionNonce != expectedNonce {
		return fmt.Errorf("session nonce mismatch: expected %s got %s", expectedNonce, claims.SessionNonce)
	}

	return b.applyClaims(claims)
}

func (b *Backend) applyClaims(claims *auth.Claims) error {
	if len(claims.Hostnames) == 0 {
		return errors.New("claims missing hostnames")
	}
	normalized, err := normalizeHostnames(claims.Hostnames)
	if err != nil {
		return err
	}
	if !sameStringSets(normalized, b.hostnames) {
		return errors.New("hostnames in token differ from registered set")
	}

	if claims.Weight != 0 && claims.Weight != b.weight {
		// Weight changes are not supported without rebalancing pools.
		return errors.New("weight change requested via token is not supported")
	}

	if val := claims.ReauthIntervalSeconds; val != nil && *val > 0 {
		b.reauthInterval = time.Duration(*val) * time.Second
	} else {
		b.reauthInterval = 0
	}

	if b.reauthInterval > 0 {
		if grace := claims.ReauthGraceSeconds; grace != nil && *grace > 0 {
			b.reauthGrace = time.Duration(*grace) * time.Second
		} else {
			b.reauthGrace = defaultReauthGrace
		}
	}

	if capVal := claims.MaintenanceGraceCapSeconds; capVal != nil {
		if *capVal < 0 {
			return errors.New("maintenance_grace_cap_seconds cannot be negative")
		}
		b.maintenanceCap = time.Duration(*capVal) * time.Second
	}

	if uri := claims.AuthorizerStatusURI; uri != "" {
		if err := validateAuthorizerStatusURI(uri); err != nil {
			return fmt.Errorf("invalid authorizer_status_uri in reauth token: %w", err)
		}
		b.authorizerStatusURI = uri
	} else {
		b.authorizerStatusURI = ""
	}
	b.policyVersion = claims.PolicyVersion
	b.maintenanceUsed = 0

	return nil
}

func (b *Backend) authorizerHealthy() (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), healthCheckTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.authorizerStatusURI, nil)
	if err != nil {
		return false, err
	}

	resp, err := b.httpClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	return resp.StatusCode == http.StatusOK, nil
}

func (b *Backend) deferForMaintenance() bool {
	capDuration := b.maintenanceCap
	if capDuration < 0 {
		capDuration = b.config.MaintenanceGraceDefault()
	}
	if capDuration == 0 {
		return false
	}
	remaining := capDuration - b.maintenanceUsed
	if remaining <= 0 {
		return false
	}

	delay := b.reauthInterval
	if delay <= 0 || delay > remaining {
		delay = remaining
	}
	jittered := b.jitterDuration(delay)
	if jittered > remaining {
		jittered = remaining
	}

	log.Printf("INFO: Deferring re-auth for backend %s by %s (maintenance window)", b.id, jittered)

	select {
	case <-time.After(jittered):
		b.maintenanceUsed += jittered
		return true
	case <-b.quit:
		return false
	}
}

func (b *Backend) prepareForNonce(nonce string) {
	b.reauthMu.Lock()
	b.pendingNonce = nonce
	b.reauthPending = true
	b.reauthMu.Unlock()
}

func (b *Backend) clearPendingNonce() {
	b.reauthMu.Lock()
	b.pendingNonce = ""
	b.reauthPending = false
	b.reauthMu.Unlock()
}

func (b *Backend) jitterDuration(base time.Duration) time.Duration {
	if base <= 0 {
		return 0
	}
	jitter := base / 5
	if jitter <= 0 {
		jitter = time.Second
	}
	n, err := cryptoRandInt64(jitter)
	if err != nil {
		return base
	}
	return base + time.Duration(n)
}

func cryptoRandInt64(max time.Duration) (int64, error) {
	if max <= 0 {
		return 0, nil
	}
	val, err := crand.Int(crand.Reader, big.NewInt(int64(max)))
	if err != nil {
		return 0, err
	}
	return val.Int64(), nil
}
