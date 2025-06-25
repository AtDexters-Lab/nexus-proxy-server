package iface

import (
	"net"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// Hub is an interface that components use to interact with the backend hub.
type Hub interface {
	GetLocalRoutes() []string
	SelectBackend(hostname string) (Backend, error)
}

// PeerManager is an interface that components use to interact with the peer manager.
type PeerManager interface {
	HandleInboundPeer(conn *websocket.Conn)
	AnnounceLocalRoutes()
	GetPeerForHostname(hostname string) (Peer, bool)
	HandleTunnelRequest(p Peer, hostname string, clientID uuid.UUID, clientIP string, connPort int)
}

// Peer represents a single connection to another Nexus node.
type Peer interface {
	Addr() string
	Send(message []byte)
	StartTunnel(conn net.Conn, hostname string)
}

// Backend represents a single connection from a backend service.
type Backend interface {
	ID() string
	AddClient(clientConn net.Conn, clientID uuid.UUID) error
	RemoveClient(clientID uuid.UUID)
	SendData(clientID uuid.UUID, data []byte) error
}
