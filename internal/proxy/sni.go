package proxy

import (
	"encoding/binary"
	"fmt"
)

const (
	recordTypeHandshake      = 0x16
	handshakeTypeClientHello = 0x01
)

// PeekServerName attempts to parse the SNI from a TLS ClientHello message.
// PeekReader is an interface that allows peeking ahead in a stream without consuming bytes.
type PeekReader interface {
	Peek(n int) ([]byte, error)
}

// PeekServerName attempts to parse the SNI from a TLS ClientHello message.
func PeekServerName(conn PeekReader) (string, error) {
	// First, peek just the 5-byte record header.
	header, err := conn.Peek(5)
	if err != nil {
		return "", fmt.Errorf("could not peek TLS record header: %w", err)
	}

	if header[0] != recordTypeHandshake {
		return "", fmt.Errorf("not a TLS handshake record")
	}

	// The record length is in bytes 3 and 4. This is the length of the
	// handshake message that follows the header.
	recLen := int(binary.BigEndian.Uint16(header[3:5]))

	// Now, peek the full handshake message. We need the original 5 bytes plus
	// the record length.
	handshake, err := conn.Peek(5 + recLen)
	if err != nil {
		// This can happen if the client sends a partial record.
		return "", fmt.Errorf("could not peek full handshake message: %w", err)
	}

	// The actual handshake data starts after the 5-byte header.
	data := handshake[5:]

	if len(data) < 4 || data[0] != handshakeTypeClientHello {
		return "", fmt.Errorf("not a ClientHello handshake")
	}

	// --- Begin parsing the ClientHello structure (RFC 5246) ---
	// Position checking throughout the parsing logic prevents panics on malformed packets.
	pos := 1 + 3 // Skip Handshake Type and Length
	if len(data) < pos+2 {
		return "", fmt.Errorf("invalid ClientHello: too short for version")
	}
	pos += 2 // Skip Protocol Version

	if len(data) < pos+32 {
		return "", fmt.Errorf("invalid ClientHello: too short for random")
	}
	pos += 32 // Skip Random

	if len(data) < pos+1 {
		return "", fmt.Errorf("invalid ClientHello: too short for session ID length")
	}
	sessionIDLen := int(data[pos])
	pos += 1 + sessionIDLen
	if len(data) < pos+2 {
		return "", fmt.Errorf("invalid ClientHello: too short for cipher suites")
	}

	cipherSuiteLen := int(binary.BigEndian.Uint16(data[pos : pos+2]))
	pos += 2 + cipherSuiteLen
	if len(data) < pos+1 {
		return "", fmt.Errorf("invalid ClientHello: too short for compression methods")
	}

	compressionMethodLen := int(data[pos])
	pos += 1 + compressionMethodLen
	if len(data) < pos+2 {
		return "", fmt.Errorf("no extensions found, SNI not available")
	}

	extensionsLen := int(binary.BigEndian.Uint16(data[pos : pos+2]))
	pos += 2

	// Boundary check for the entire extensions block.
	extEnd := pos + extensionsLen
	if extEnd > len(data) {
		return "", fmt.Errorf("invalid ClientHello: extensions length exceeds packet size")
	}

	// Iterate through extensions to find the SNI (type 0).
	for pos < extEnd {
		if pos+4 > extEnd {
			break
		} // Malformed extension, not enough data for header.
		extType := binary.BigEndian.Uint16(data[pos : pos+2])
		extLen := int(binary.BigEndian.Uint16(data[pos+2 : pos+4]))
		pos += 4

		if extType == 0 { // This is the SNI extension.
			if pos+extLen > extEnd {
				return "", fmt.Errorf("invalid SNI extension length")
			}

			// SNI data: 2 bytes list length, then list of names.
			if extLen < 2 {
				return "", fmt.Errorf("invalid SNI extension data: too short for list length")
			}
			sniListLen := int(binary.BigEndian.Uint16(data[pos : pos+2]))
			if pos+2+sniListLen > extEnd {
				return "", fmt.Errorf("invalid SNI list length")
			}

			// Move to the first name entry in the list.
			pos += 2
			if pos+3 > extEnd {
				return "", fmt.Errorf("invalid SNI name entry: too short")
			}
			nameType := data[pos]
			if nameType != 0 { // We only support the 'host_name' type.
				pos += extLen - 2 // Skip this entire entry.
				continue
			}
			pos += 1

			nameLen := int(binary.BigEndian.Uint16(data[pos : pos+2]))
			pos += 2
			if pos+nameLen > extEnd {
				return "", fmt.Errorf("invalid SNI name length")
			}

			// Successfully parsed the server name (FQDN).
			serverName := string(data[pos : pos+nameLen])
			return serverName, nil
		}

		pos += extLen
	}

	return "", fmt.Errorf("SNI extension not found")
}
