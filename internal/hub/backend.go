package hub

import (
	"encoding/json"
	"fmt"
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
	maxMessageSize = 32*1024 + protocol.MessageHeaderLength // This must be sent within writeWait
)

// Backend represents a single, authenticated WebSocket connection from a backend service.
type Backend struct {
	id          string
	conn        *websocket.Conn
	config      *config.Config
	hostnames   []string
	weight      int
	clients     sync.Map
	dataForSelf chan []byte
	quit        chan struct{}
	closeOnce   sync.Once
	isClosed    bool // Indicates if the backend is closed
}

// NewBackend creates a new Backend instance.
func NewBackend(conn *websocket.Conn, hostnames []string, weight int, cfg *config.Config) *Backend {
	return &Backend{
		id:          uuid.New().String(),
		conn:        conn,
		config:      cfg,
		hostnames:   hostnames,
		weight:      weight,
		dataForSelf: make(chan []byte, 256),
		quit:        make(chan struct{}),
		closeOnce:   sync.Once{},
		isClosed:    false,
	}
}

func (b *Backend) ID() string {
	return b.id
}

func (b *Backend) Close() {
	b.closeOnce.Do(func() {
		b.quit <- struct{}{} // Signal the pumps to stop.
		b.conn.Close()
		b.clients.Range(func(key, value interface{}) bool {
			if clientConn, ok := value.(net.Conn); ok {
				clientConn.Close()
			}
			return true
		})
		close(b.dataForSelf)
		b.isClosed = true
		close(b.quit)
	})
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

func (b *Backend) AddClient(clientConn net.Conn, clientID uuid.UUID, hostname string, isTLS bool) error {
	var connPort int
	if tcpAddr, ok := clientConn.LocalAddr().(*net.TCPAddr); ok {
		connPort = tcpAddr.Port
	} else {
		// Handle cases where it might not be a TCP connection, though it always should be.
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

	err := b.SendControlMessage(msg)
	if err != nil {
		return fmt.Errorf("failed to send connect message for client %s: %w", clientID, err)
	}
	b.clients.Store(clientID, clientConn)
	return nil
}

func (b *Backend) RemoveClient(clientID uuid.UUID) {
	if _, ok := b.clients.Load(clientID); ok {
		b.clients.Delete(clientID)
		msg := protocol.ControlMessage{Event: protocol.EventDisconnect, ClientID: clientID}
		err := b.SendControlMessage(msg)
		if err != nil {
			log.Printf("ERROR: Failed to send disconnect message for client %s: %v", clientID, err)
		} else {
			log.Printf("INFO: Client %s disconnected from backend %s", clientID, b.id)
		}
	}
}

func (b *Backend) SendData(clientID uuid.UUID, data []byte) error {
	// First, check if the backend is shutting down to fail fast.
	select {
	case <-b.quit:
		return fmt.Errorf("backend %s is closing", b.id)
	default:
	}
	if b.isClosed {
		return fmt.Errorf("backend %s is already closed", b.id)
	}

	header := make([]byte, 1+protocol.ClientIDLength)
	header[0] = protocol.ControlByteData
	copy(header[1:], clientID[:])

	message := append(header, data...)
	// Use a non-blocking send to avoid a slow backend blocking the clients and thereby increasing memory consumption of proxy.
	select {
	case b.dataForSelf <- message:
		return nil
	default:
		return fmt.Errorf("backend %s send channel full, dropping data for client %s", b.id, clientID)
	}
}

func (b *Backend) SendControlMessage(msg protocol.ControlMessage) error {
	// First, check if the backend is shutting down to fail fast.
	select {
	case <-b.quit:
		return fmt.Errorf("backend %s is closing", b.id)
	default:
	}
	if b.isClosed {
		return fmt.Errorf("backend %s is already closed", b.id)
	}

	payload, err := json.Marshal(msg)
	if err != nil {
		log.Printf("ERROR: Failed to marshal control message for backend %s: %v", b.id, err)
		return err
	}

	header := make([]byte, 1)
	header[0] = protocol.ControlByteControl

	message := append(header, payload...)
	select {
	case b.dataForSelf <- message:
		return nil
	default:
		return fmt.Errorf("backend %s send channel full, dropping data for client %s", b.id, msg.ClientID)
	}
}

func (b *Backend) readPump() {
	defer func() {
		log.Printf("INFO: Closing backend %s read pump", b.id)
		b.Close()
	}()
	b.conn.SetReadLimit(maxMessageSize)
	b.conn.SetReadDeadline(time.Now().Add(pongWait))
	b.conn.SetPongHandler(func(string) error {
		b.conn.SetReadDeadline(time.Now().Add(pongWait)) // Reset the read deadline after receiving a pong.
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

		b.conn.SetReadDeadline(time.Now().Add(pongWait)) // Reset the read deadline after a successful read.

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
				log.Printf("WARN: Malformed data message from backend %s", b.id)
				continue
			}
			var clientID uuid.UUID
			copy(clientID[:], payload[:protocol.ClientIDLength])
			data := payload[protocol.ClientIDLength:]

			rawConn, ok := b.clients.Load(clientID)
			if ok {
				clientConn, ok := rawConn.(net.Conn)
				if ok {
					if b.config.IdleTimeout() > 0 {
						clientConn.SetWriteDeadline(time.Now().Add(b.config.IdleTimeout()))
					}
					if _, err := clientConn.Write(data); err != nil {
						log.Printf("WARN: Failed to write to client %s: %v. Closing.", clientID, err)
						clientConn.Close()
					}
				} else {
					log.Printf("ERROR: Client %s is not a net.Conn type in backend %s", clientID, b.id)
				}
			} else {
				log.Printf("WARN: Received data for unknown client %s from backend %s. Ignoring.", clientID, b.id)
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
			err := b.SendControlMessage(pongMsg)
			if err != nil {
				log.Printf("ERROR: Failed to send pong for client %s to backend %s: %v", msg.ClientID, b.id, err)
			}
		} else {
			log.Printf("INFO: Client %s is no longer active, not sending pong to backend %s", msg.ClientID, b.id)
		}
	case protocol.EventDisconnect:
		// This happens if the local service connection closes first.
		if rawConn, ok := b.clients.Load(msg.ClientID); ok {
			if conn, ok := rawConn.(net.Conn); ok {
				log.Printf("INFO: Backend %s reported disconnect for client %s. Closing client connection.", b.id, msg.ClientID)
				b.clients.Delete(msg.ClientID)
				conn.Close() // conn is closed after client removal so that the RemoveClient call does not try to close it again.
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
		case message, ok := <-b.dataForSelf:
			b.conn.SetWriteDeadline(time.Now().Add(writeWait))

			if !ok {
				b.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			if err := b.conn.WriteMessage(websocket.BinaryMessage, message); err != nil {
				var msgPrint = message
				if len(msgPrint) > 100 {
					msgPrint = append(msgPrint[:100], []byte("...")...)
				}
				log.Printf("ERROR: Failed to write to backend %s: %v | %s", b.id, err, string(msgPrint))
				return
			}
		case <-ticker.C:
			b.conn.SetWriteDeadline(time.Now().Add(writeWait))
			// Send a ping to keep the connection alive.
			if err := b.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}
