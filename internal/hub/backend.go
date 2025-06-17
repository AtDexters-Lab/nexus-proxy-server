package hub

import (
	"encoding/json"
	"log"
	"net"
	"sync"
	"time"

	"github.com/AtDexters-Lab/nexus-proxy-server/internal/config"
	"github.com/AtDexters-Lab/nexus-proxy-server/internal/protocol"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

const (
	writeWait      = 10 * time.Second
	pongWait       = 60 * time.Second
	pingPeriod     = (pongWait * 9) / 10
	maxMessageSize = 8192
)

// Backend represents a single, authenticated WebSocket connection from a backend service.
type Backend struct {
	id          string
	conn        *websocket.Conn
	config      *config.Config
	hostname    string
	weight      int
	connMu      sync.Mutex
	clients     sync.Map
	dataForSelf chan []byte
}

// NewBackend creates a new Backend instance.
func NewBackend(conn *websocket.Conn, hostname string, weight int, cfg *config.Config) *Backend {
	return &Backend{
		id:          uuid.New().String(),
		conn:        conn,
		config:      cfg,
		hostname:    hostname,
		weight:      weight,
		dataForSelf: make(chan []byte, 256),
	}
}

func (b *Backend) ID() string {
	return b.id
}

func (b *Backend) Close() {
	b.conn.Close()
	b.clients.Range(func(key, value interface{}) bool {
		if clientConn, ok := value.(net.Conn); ok {
			clientConn.Close()
		}
		return true
	})
	close(b.dataForSelf)
}

func (b *Backend) StartPumps() {
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		b.writePump()
	}()
	go func() {
		defer wg.Done()
		b.readPump()
	}()

	wg.Wait()
}

func (b *Backend) AddClient(clientConn net.Conn, clientID uuid.UUID) {
	b.clients.Store(clientID, clientConn)
	var connPort int
	if tcpAddr, ok := clientConn.LocalAddr().(*net.TCPAddr); ok {
		connPort = tcpAddr.Port
	} else {
		// Handle cases where it might not be a TCP connection, though it always should be.
		log.Printf("WARN: Could not determine destination port for client %s", clientID)
	}
	msg := protocol.ControlMessage{
		Event:    protocol.EventConnect,
		ClientID: clientID,
		ConnPort: connPort,
		ClientIP: clientConn.RemoteAddr().String(),
	}

	b.SendControlMessage(msg)
}

func (b *Backend) RemoveClient(clientID uuid.UUID) {
	if _, ok := b.clients.Load(clientID); ok {
		b.clients.Delete(clientID)
		msg := protocol.ControlMessage{Event: protocol.EventDisconnect, ClientID: clientID}
		b.SendControlMessage(msg)
	}
}

func (b *Backend) SendData(clientID uuid.UUID, data []byte) {
	header := make([]byte, 1+protocol.ClientIDLength)
	header[0] = protocol.ControlByteData
	copy(header[1:], clientID[:])

	message := append(header, data...)
	// Use a non-blocking send to avoid a slow client blocking the proxy.
	select {
	case b.dataForSelf <- message:
	default:
		log.Printf("WARN: Backend %s send channel full. Dropping data for client %s.", b.id, clientID)
	}
}

func (b *Backend) SendControlMessage(msg protocol.ControlMessage) {
	payload, err := json.Marshal(msg)
	if err != nil {
		log.Printf("ERROR: Failed to marshal control message for backend %s: %v", b.id, err)
		return
	}

	header := make([]byte, 1)
	header[0] = protocol.ControlByteControl

	message := append(header, payload...)
	b.dataForSelf <- message
}

func (b *Backend) readPump() {
	defer b.Close()
	b.conn.SetReadLimit(maxMessageSize)
	b.conn.SetReadDeadline(time.Now().Add(pongWait))
	b.conn.SetPongHandler(func(string) error {
		b.conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

	for {
		_, message, err := b.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("ERROR: Unexpected close error from backend %s: %v", b.id, err)
			}
			break
		}

		if len(message) < 1 {
			continue
		}

		controlByte := message[0]
		payload := message[1:]

		switch controlByte {
		case protocol.ControlByteControl:
			var msg protocol.ControlMessage
			if err := json.Unmarshal(payload, &msg); err != nil {
				log.Printf("WARN: Malformed control message from backend %s", b.id)
				continue
			}
			b.handleControlMessage(msg)

		case protocol.ControlByteData:
			if len(payload) < protocol.ClientIDLength {
				continue
			}
			var clientID uuid.UUID
			copy(clientID[:], payload[:protocol.ClientIDLength])
			data := payload[protocol.ClientIDLength:]

			if rawConn, ok := b.clients.Load(clientID); ok {
				if clientConn, ok := rawConn.(net.Conn); ok {
					if b.config.IdleTimeout() > 0 {
						clientConn.SetWriteDeadline(time.Now().Add(b.config.IdleTimeout()))
					}
					if _, err := clientConn.Write(data); err != nil {
						log.Printf("WARN: Failed to write to client %s: %v. Closing.", clientID, err)
						clientConn.Close()
					}
				}
			}
		}
	}
}

func (b *Backend) handleControlMessage(msg protocol.ControlMessage) {
	switch msg.Event {
	case protocol.EventPingClient:
		log.Printf("DEBUG: Received ping for client %s from backend %s", msg.ClientID, b.id)
		// Check if the client is still considered active by the proxy.
		if _, ok := b.clients.Load(msg.ClientID); ok {
			pongMsg := protocol.ControlMessage{Event: protocol.EventPongClient, ClientID: msg.ClientID}
			b.SendControlMessage(pongMsg)
		} else {
			log.Printf("INFO: Client %s is no longer active, not sending pong to backend %s", msg.ClientID, b.id)
		}
	case protocol.EventDisconnect:
		// This happens if the local service connection closes first.
		if rawConn, ok := b.clients.Load(msg.ClientID); ok {
			if conn, ok := rawConn.(net.Conn); ok {
				log.Printf("INFO: Backend %s reported disconnect for client %s. Closing client connection.", b.id, msg.ClientID)
				conn.Close()
				b.clients.Delete(msg.ClientID)
			}
		}
	default:
		log.Printf("WARN: Unknown control event '%s' from backend %s", msg.Event, b.id)
	}
}

func (b *Backend) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		b.Close()
	}()
	for {
		select {
		case message, ok := <-b.dataForSelf:
			b.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				b.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			if err := b.conn.WriteMessage(websocket.BinaryMessage, message); err != nil {
				log.Printf("ERROR: Failed to write to backend %s: %v", b.id, err)
				return
			}
		case <-ticker.C:
			b.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := b.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}
