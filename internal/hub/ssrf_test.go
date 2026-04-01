package hub

import (
	"net"
	"testing"
)

func TestRejectPrivateIP(t *testing.T) {
	rejected := []string{
		"127.0.0.1",
		"10.0.0.1",
		"172.16.0.1",
		"192.168.1.1",
		"169.254.169.254",
		"::1",
		"fe80::1",
		"0.0.0.0",
	}
	for _, s := range rejected {
		ip := net.ParseIP(s)
		if err := rejectPrivateIP(ip); err == nil {
			t.Errorf("expected %s to be rejected", s)
		}
	}

	allowed := []string{
		"8.8.8.8",
		"1.1.1.1",
		"93.184.216.34",
	}
	for _, s := range allowed {
		ip := net.ParseIP(s)
		if err := rejectPrivateIP(ip); err != nil {
			t.Errorf("expected %s to be allowed, got: %v", s, err)
		}
	}
}
