package peer

import (
	"context"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/AtDexters-Lab/nexus-proxy-server/internal/config"
	"github.com/AtDexters-Lab/nexus-proxy-server/internal/protocol"
	"github.com/AtDexters-Lab/nexus-proxy-server/internal/proxy"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

const (
	writeWait      = 10 * time.Second
	pongWait       = 60 * time.Second
	pingPeriod     = (pongWait * 9) / 10
	reconnectDelay = 5 * time.Second
)

type peerImpl struct {
	addr    string
	conn    *websocket.Conn
	config  *config.Config
	manager *Manager
	send    chan []byte
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

// Connect attempts to establish an outbound WebSocket connection to the peer.
func (p *peerImpl) Connect(ctx context.Context) {
	headers := http.Header{}
	headers.Add("X-Nexus-Secret", p.config.PeerSecret)

	for {
		select {
		case <-ctx.Done():
			return
		default:
			log.Printf("INFO: [PEER] Attempting to connect to %s", p.addr)
			conn, _, err := websocket.DefaultDialer.Dial(p.addr, headers)
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
func (p *peerImpl) Send(message []byte) {
	// A non-blocking send to the channel.
	select {
	case p.send <- message:
	default:
		log.Printf("WARN: [PEER] Send channel for peer %s is full. Dropping message.", p.addr)
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
				p.manager.HandleTunnelRequest(p, msg.Hostname, msg.ClientID)
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
					continue
				}
				var clientID uuid.UUID
				copy(clientID[:], message[1:1+protocol.ClientIDLength])
				payload := message[1+protocol.ClientIDLength:]
				p.manager.HandleTunnelData(clientID, payload)

			case protocol.PeerTunnelClose:
				if len(message) < 1+protocol.ClientIDLength {
					continue
				}
				var clientID uuid.UUID
				copy(clientID[:], message[1:1+protocol.ClientIDLength])
				p.manager.HandleTunnelClose(clientID)

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
			// A more robust way might be to wrap messages in a struct, but this works.
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

// StartTunnel initiates the tunneling of a client connection to this peer.
func (p *peerImpl) StartTunnel(conn net.Conn, hostname string) {
	defer conn.Close()

	clientID := uuid.New()

	// 1. Send the tunnel request to the peer (as a JSON text message).
	req := protocol.PeerMessage{
		Type:     protocol.PeerTunnelRequest,
		Hostname: hostname,
		ClientID: clientID,
	}
	payload, _ := json.Marshal(req)
	p.Send(payload)

	// 2. Start proxying data from the client to the peer (as binary messages).
	bufPtr := proxy.GetBuffer()
	defer proxy.PutBuffer(bufPtr)
	buf := *bufPtr

	for {
		n, err := conn.Read(buf)
		if err != nil {
			break
		}

		header := make([]byte, 1+protocol.ClientIDLength)
		header[0] = protocol.PeerTunnelData
		copy(header[1:], clientID[:])
		message := append(header, buf[:n]...)
		p.Send(message)
	}

	// 3. Send a close message when the client disconnects.
	closeHeader := make([]byte, 1+protocol.ClientIDLength)
	closeHeader[0] = protocol.PeerTunnelClose
	copy(closeHeader[1:], clientID[:])
	p.Send(closeHeader)
}
