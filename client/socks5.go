package client

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strconv"
)

// SOCKS5 constants (RFC 1928).
const (
	socks5Version     = 0x05
	socks5AuthNone    = 0x00
	socks5AuthNoAccpt = 0xFF
	socks5CmdConnect  = 0x01
	socks5AtypIPv4    = 0x01
	socks5AtypDomain  = 0x03
	socks5AtypIPv6    = 0x04

	socks5RepSuccess         = 0x00
	socks5RepGeneralFailure  = 0x01
	socks5RepNotAllowed      = 0x02
	socks5RepConnRefused     = 0x05
	socks5RepCmdNotSupported = 0x07
	socks5RepAddrNotSupp     = 0x08
)

// socks5Handshake performs the SOCKS5 method negotiation. Only the
// no-authentication method (0x00) is supported.
func socks5Handshake(conn net.Conn) error {
	// +----+----------+----------+
	// |VER | NMETHODS | METHODS  |
	// +----+----------+----------+
	var header [2]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		return fmt.Errorf("read method header: %w", err)
	}
	if header[0] != socks5Version {
		return fmt.Errorf("unsupported SOCKS version %d", header[0])
	}
	nMethods := int(header[1])
	if nMethods == 0 {
		return fmt.Errorf("no authentication methods offered")
	}
	methods := make([]byte, nMethods)
	if _, err := io.ReadFull(conn, methods); err != nil {
		return fmt.Errorf("read methods: %w", err)
	}

	hasNoAuth := false
	for _, m := range methods {
		if m == socks5AuthNone {
			hasNoAuth = true
			break
		}
	}

	if !hasNoAuth {
		// Reject: no acceptable method.
		_, _ = conn.Write([]byte{socks5Version, socks5AuthNoAccpt})
		return fmt.Errorf("no supported authentication method")
	}

	// Accept no-auth.
	_, err := conn.Write([]byte{socks5Version, socks5AuthNone})
	return err
}

// socks5ReadConnect reads a SOCKS5 CONNECT request and returns the target
// host and port. Only the CONNECT command is supported.
func socks5ReadConnect(conn net.Conn) (host string, port int, err error) {
	// +----+-----+-------+------+----------+----------+
	// |VER | CMD |  RSV  | ATYP | DST.ADDR | DST.PORT |
	// +----+-----+-------+------+----------+----------+
	var header [4]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		return "", 0, fmt.Errorf("read request header: %w", err)
	}
	if header[0] != socks5Version {
		return "", 0, fmt.Errorf("unsupported SOCKS version %d", header[0])
	}
	if header[1] != socks5CmdConnect {
		_ = socks5SendReply(conn, socks5RepCmdNotSupported)
		return "", 0, fmt.Errorf("unsupported command %d", header[1])
	}

	atyp := header[3]
	switch atyp {
	case socks5AtypIPv4:
		var addr [4]byte
		if _, err := io.ReadFull(conn, addr[:]); err != nil {
			return "", 0, fmt.Errorf("read IPv4 addr: %w", err)
		}
		host = net.IP(addr[:]).String()
	case socks5AtypDomain:
		var lenBuf [1]byte
		if _, err := io.ReadFull(conn, lenBuf[:]); err != nil {
			return "", 0, fmt.Errorf("read domain length: %w", err)
		}
		domainLen := int(lenBuf[0])
		if domainLen == 0 {
			return "", 0, fmt.Errorf("empty domain name")
		}
		domain := make([]byte, domainLen)
		if _, err := io.ReadFull(conn, domain); err != nil {
			return "", 0, fmt.Errorf("read domain: %w", err)
		}
		host = string(domain)
	case socks5AtypIPv6:
		var addr [16]byte
		if _, err := io.ReadFull(conn, addr[:]); err != nil {
			return "", 0, fmt.Errorf("read IPv6 addr: %w", err)
		}
		host = net.IP(addr[:]).String()
	default:
		_ = socks5SendReply(conn, socks5RepAddrNotSupp)
		return "", 0, fmt.Errorf("unsupported address type %d", atyp)
	}

	var portBuf [2]byte
	if _, err := io.ReadFull(conn, portBuf[:]); err != nil {
		return "", 0, fmt.Errorf("read port: %w", err)
	}
	port = int(binary.BigEndian.Uint16(portBuf[:]))
	return host, port, nil
}

// socks5SendReply sends a SOCKS5 reply to the client. The bind address is
// always 0.0.0.0:0.
func socks5SendReply(conn net.Conn, rep byte) error {
	// +----+-----+-------+------+----------+----------+
	// |VER | REP |  RSV  | ATYP | BND.ADDR | BND.PORT |
	// +----+-----+-------+------+----------+----------+
	reply := []byte{
		socks5Version, rep, 0x00,
		socks5AtypIPv4, 0, 0, 0, 0, // 0.0.0.0
		0, 0, // port 0
	}
	_, err := conn.Write(reply)
	return err
}

// socks5TargetAddr formats host and port into a host:port string suitable
// for use as a TargetAddr in an EventOutboundConnect control message.
func socks5TargetAddr(host string, port int) string {
	return net.JoinHostPort(host, strconv.Itoa(port))
}
