package protocol

import "github.com/google/uuid"

const (
	// ClientIDLength is the expected length of a client's unique identifier (UUID).
	ClientIDLength = 16
	// ControlByteData indicates a standard data message.
	ControlByteData byte = 0x01
	// ControlByteControl indicates a JSON control message.
	ControlByteControl byte = 0x02
)

// EventType defines the type of a control message event.
type EventType string

const (
	// EventConnect is sent to a backend when a new client connects.
	EventConnect EventType = "connect"
	// EventDisconnect is sent to a backend when a client disconnects.
	EventDisconnect EventType = "disconnect"
)

// ControlMessage defines the structure for out-of-band communication
// between the proxy and the backend.
type ControlMessage struct {
	Event    EventType `json:"event"`
	ClientID uuid.UUID `json:"client_id"`
}
