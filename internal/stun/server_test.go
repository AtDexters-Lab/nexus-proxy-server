package stun

import (
	"net"
	"testing"
	"time"

	pionstun "github.com/pion/stun/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func startTestServer(t *testing.T) *Server {
	t.Helper()
	s := New(0)
	go s.Run()
	t.Cleanup(s.Stop)

	require.Eventually(t, func() bool {
		return s.LocalAddr() != nil
	}, 2*time.Second, 10*time.Millisecond, "STUN server did not start in time")

	return s
}

func assertPacketDropped(t *testing.T, s *Server, payload []byte) {
	t.Helper()

	conn, err := net.DialUDP("udp", nil, s.LocalAddr().(*net.UDPAddr))
	require.NoError(t, err)
	defer conn.Close()

	_, err = conn.Write(payload)
	require.NoError(t, err)

	conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	buf := make([]byte, 1500)
	_, err = conn.Read(buf)
	assert.Error(t, err) // timeout expected
}

func TestBindingRequest_ReturnsXORMappedAddress(t *testing.T) {
	s := startTestServer(t)

	conn, err := net.DialUDP("udp", nil, s.LocalAddr().(*net.UDPAddr))
	require.NoError(t, err)
	defer conn.Close()

	msg, err := pionstun.Build(pionstun.TransactionID, pionstun.BindingRequest)
	require.NoError(t, err)

	_, err = conn.Write(msg.Raw)
	require.NoError(t, err)

	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1500)
	n, err := conn.Read(buf)
	require.NoError(t, err)

	var resp pionstun.Message
	require.NoError(t, resp.UnmarshalBinary(buf[:n]))

	assert.Equal(t, pionstun.NewType(pionstun.MethodBinding, pionstun.ClassSuccessResponse), resp.Type)
	assert.Equal(t, msg.TransactionID, resp.TransactionID)

	var xorAddr pionstun.XORMappedAddress
	require.NoError(t, xorAddr.GetFrom(&resp))

	clientAddr := conn.LocalAddr().(*net.UDPAddr)
	assert.Equal(t, clientAddr.Port, xorAddr.Port)
	assert.True(t, xorAddr.IP.IsLoopback(), "expected loopback IP, got %s", xorAddr.IP)
}

func TestNonStunPacket_Dropped(t *testing.T) {
	s := startTestServer(t)
	assertPacketDropped(t, s, []byte("hello world"))
}

func TestMalformedStunPacket_Dropped(t *testing.T) {
	s := startTestServer(t)
	// Valid magic cookie at offset 4 but truncated (8 bytes instead of 20).
	assertPacketDropped(t, s, []byte{0x00, 0x01, 0x00, 0x00, 0x21, 0x12, 0xA4, 0x42})
}

func TestStopBeforeRun_NoPanic(t *testing.T) {
	s := New(0)
	s.Stop()
}
