package proxy_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/AtDexters-Lab/nexus-proxy-server/internal/proxy"
	"github.com/stretchr/testify/require"
)

// Mock PeekableConn for testing
type mockPeekableConn struct {
	data []byte
	err  error
}

func (m *mockPeekableConn) Peek(n int) ([]byte, error) {
	if m.err != nil {
		return nil, m.err
	}
	if n > len(m.data) {
		return nil, errors.New("not enough data")
	}
	return m.data[:n], nil
}

func makeClientHelloWithSNI(serverName string) []byte {
	// Build a minimal TLS ClientHello with SNI extension
	var buf bytes.Buffer

	// TLS record header
	buf.WriteByte(0x16)           // Handshake
	buf.Write([]byte{0x03, 0x01}) // Version TLS 1.0
	// We'll fill in the record length later
	buf.Write([]byte{0x00, 0x00})

	handshakeStart := buf.Len()

	// Handshake header
	buf.WriteByte(0x01)                 // ClientHello
	buf.Write([]byte{0x00, 0x00, 0x00}) // Length (to fill later)

	// Version
	buf.Write([]byte{0x03, 0x03}) // TLS 1.2

	// Random
	buf.Write(make([]byte, 32))

	// Session ID
	buf.WriteByte(0x00)

	// Cipher Suites
	buf.Write([]byte{0x00, 0x02, 0x00, 0x2f}) // length=2, TLS_RSA_WITH_AES_128_CBC_SHA

	// Compression Methods
	buf.WriteByte(0x01)
	buf.WriteByte(0x00)

	// Extensions
	extStart := buf.Len()
	buf.Write([]byte{0x00, 0x00}) // Extensions length (to fill later)

	// SNI extension
	sniExtStart := buf.Len()
	buf.Write([]byte{0x00, 0x00}) // Extension type: SNI
	// Extension length (to fill later)
	buf.Write([]byte{0x00, 0x00})

	// SNI list length
	sniList := bytes.Buffer{}
	sniList.WriteByte(0x00) // name_type: host_name
	binary.Write(&sniList, binary.BigEndian, uint16(len(serverName)))
	sniList.WriteString(serverName)
	sniListBytes := sniList.Bytes()
	binary.Write(&buf, binary.BigEndian, uint16(len(sniListBytes)))
	buf.WriteByte(0x00) // name_type: host_name
	binary.Write(&buf, binary.BigEndian, uint16(len(serverName)))
	buf.WriteString(serverName)

	// Fix SNI extension length
	sniExtLen := buf.Len() - sniExtStart - 4
	binary.BigEndian.PutUint16(buf.Bytes()[sniExtStart+2:sniExtStart+4], uint16(sniExtLen))

	// Fix extensions length
	extLen := buf.Len() - extStart - 2
	binary.BigEndian.PutUint16(buf.Bytes()[extStart:extStart+2], uint16(extLen))

	// Fix handshake length
	handshakeLen := buf.Len() - handshakeStart
	buf.Bytes()[handshakeStart-3] = byte(handshakeLen >> 16)
	buf.Bytes()[handshakeStart-2] = byte(handshakeLen >> 8)
	buf.Bytes()[handshakeStart-1] = byte(handshakeLen)

	// Fix record length
	recLen := buf.Len() - 5
	binary.BigEndian.PutUint16(buf.Bytes()[3:5], uint16(recLen))

	return buf.Bytes()
}

func TestPeekServerName_Success(t *testing.T) {
	serverName := "example.com"
	clientHello := makeClientHelloWithSNI(serverName)
	conn := &mockPeekableConn{data: clientHello}
	name, err := proxy.PeekServerName(conn)
	require.NoError(t, err)
	require.Equal(t, serverName, name)
}

func TestPeekServerName_NoSNI(t *testing.T) {
	// ClientHello without SNI extension
	clientHello := makeClientHelloWithSNI("")
	// Remove SNI extension
	clientHello = clientHello[:len(clientHello)-len("example.com")-7]
	conn := &mockPeekableConn{data: clientHello}
	_, err := proxy.PeekServerName(conn)
	require.Error(t, err)
	// TODO
	// require.Contains(t, err.Error(), "SNI extension not found")
}

func TestPeekServerName_NotTLSHandshake(t *testing.T) {
	data := []byte{0x15, 0x03, 0x01, 0x00, 0x10}
	conn := &mockPeekableConn{data: data}
	_, err := proxy.PeekServerName(conn)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not a TLS handshake record")
}

func TestPeekServerName_PeekError(t *testing.T) {
	conn := &mockPeekableConn{err: errors.New("peek error")}
	_, err := proxy.PeekServerName(conn)
	require.Error(t, err)
	require.Contains(t, err.Error(), "could not peek TLS record header")
}

func TestPeekServerName_PartialRecord(t *testing.T) {
	clientHello := makeClientHelloWithSNI("example.com")
	conn := &mockPeekableConn{data: clientHello[:7]}
	_, err := proxy.PeekServerName(conn)
	require.Error(t, err)
	require.Contains(t, err.Error(), "could not peek full handshake message")
}
