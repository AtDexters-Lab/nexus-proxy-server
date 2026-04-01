package protocol

import "testing"

func TestRouteKey(t *testing.T) {
	tests := []struct {
		transport Transport
		port      int
		want      string
	}{
		{TransportTCP, 443, "tcp:443"},
		{TransportTCP, 80, "tcp:80"},
		{TransportUDP, 53, "udp:53"},
		{TransportUDP, 0, "udp:0"},
		{"", 8080, "tcp:8080"}, // unknown transport defaults to TCP
	}
	for _, tt := range tests {
		got := RouteKey(tt.transport, tt.port)
		if got != tt.want {
			t.Errorf("RouteKey(%q, %d) = %q, want %q", tt.transport, tt.port, got, tt.want)
		}
	}
}
