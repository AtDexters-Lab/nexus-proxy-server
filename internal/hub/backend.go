package hub

import (
	"encoding/json"
	"log"
	"net"
	"sync"
	"time"

	"github.com/AtDexters-Lab/global-access-relay/internal/protocol"
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
	hostname    string
	weight      int
	connMu      sync.Mutex
	clients     sync.Map
	dataForSelf chan []byte
}

// NewBackend creates a new Backend instance.
func NewBackend(conn *websocket.Conn, hostname string, weight int) *Backend {
	return &Backend{
		id:          uuid.New().String(),
		conn:        conn,
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
	msg := protocol.ControlMessage{Event: protocol.EventConnect, ClientID: clientID}
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
	b.dataForSelf <- message
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

		if len(message) < 1+protocol.ClientIDLength {
			log.Printf("WARN: Received malformed message from backend %s: too short", b.id)
			continue
		}

		var clientID uuid.UUID
		copy(clientID[:], message[1:1+protocol.ClientIDLength])

		payload := message[1+protocol.ClientIDLength:]

		if rawConn, ok := b.clients.Load(clientID); ok {
			if clientConn, ok := rawConn.(net.Conn); ok {
				if b.config.IdleTimeout() > 0 {
					clientConn.SetWriteDeadline(time.Now().Add(b.config.IdleTimeout()))
				}
				if _, err := clientConn.Write(payload); err != nil {
					log.Printf("WARN: Failed to write to client %s: %v. Closing.", clientID, err)
					clientConn.Close()
				}
			}
		} else {
			log.Printf("WARN: Received data from backend %s for unknown client %s", b.id, clientID)
		}
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
