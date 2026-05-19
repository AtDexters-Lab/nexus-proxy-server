package protocol

import (
	"encoding/json"
	"testing"
)

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

func TestControlMessage_OutboundFields(t *testing.T) {
	msg := ControlMessage{
		Event:      EventOutboundConnect,
		TargetAddr: "example.com:443",
	}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	var decoded ControlMessage
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Event != EventOutboundConnect {
		t.Errorf("event = %q, want %q", decoded.Event, EventOutboundConnect)
	}
	if decoded.TargetAddr != "example.com:443" {
		t.Errorf("target_addr = %q, want %q", decoded.TargetAddr, "example.com:443")
	}

	// Test result message
	result := ControlMessage{
		Event:   EventOutboundResult,
		Success: true,
	}
	data, _ = json.Marshal(result)
	var decodedResult ControlMessage
	_ = json.Unmarshal(data, &decodedResult)
	if !decodedResult.Success {
		t.Error("expected Success=true")
	}
}

func TestBackendClaims_OutboundFields(t *testing.T) {
	claims := BackendClaims{
		OutboundAllowed:      true,
		AllowedOutboundPorts: []int{25, 443},
	}
	data, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	var decoded BackendClaims
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if !decoded.OutboundAllowed {
		t.Error("expected OutboundAllowed=true")
	}
	if len(decoded.AllowedOutboundPorts) != 2 || decoded.AllowedOutboundPorts[0] != 25 || decoded.AllowedOutboundPorts[1] != 443 {
		t.Errorf("unexpected AllowedOutboundPorts: %v", decoded.AllowedOutboundPorts)
	}
}

func TestAttestationResultMessage_RoundTrip(t *testing.T) {
	five := 5
	orig := AttestationResultMessage{
		Type: AttestationResultHandshake,
		Accepted: AcceptedClaims{
			Hostnames:            []string{"a.example.com", "b.example.com"},
			TCPPorts:             []int{443, 80},
			UDPRoutes:            []UDPRouteClaim{{Port: 53, FlowIdleTimeoutSeconds: &five}},
			OutboundAllowed:      true,
			AllowedOutboundPorts: []int{443},
		},
		Rejected: []RejectedClaim{
			{Kind: RejectedKindTCPPort, Code: RejectedCodeTCPPortNotAllowed, Value: "2222", Reason: "not in allowlist"},
		},
		Truncated: true,
	}
	data, err := json.Marshal(orig)
	if err != nil {
		t.Fatal(err)
	}
	var decoded AttestationResultMessage
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Type != AttestationResultHandshake {
		t.Errorf("type = %q", decoded.Type)
	}
	if len(decoded.Rejected) != 1 || decoded.Rejected[0].Code != RejectedCodeTCPPortNotAllowed {
		t.Errorf("rejected mismatch: %+v", decoded.Rejected)
	}
	if !decoded.Truncated {
		t.Error("truncated lost")
	}
	if len(decoded.Accepted.TCPPorts) != 2 {
		t.Errorf("tcp_ports lost: %v", decoded.Accepted.TCPPorts)
	}
}

// TestHubBackendTextFrames_HaveTypeField enforces the discriminator contract:
// every hub→backend text-frame type has a non-empty "type" field that survives
// JSON round-trip. The contract is documented in protocol.go and consumed by
// client/handleTextMessage's envelope-decode pattern.
func TestHubBackendTextFrames_HaveTypeField(t *testing.T) {
	tests := []struct {
		name string
		msg  interface{}
	}{
		{"ChallengeMessage_handshake", ChallengeMessage{Type: ChallengeHandshake, Nonce: "n"}},
		{"ChallengeMessage_reauth", ChallengeMessage{Type: ChallengeReauth, Nonce: "n"}},
		{"AttestationResult_handshake", AttestationResultMessage{Type: AttestationResultHandshake}},
		{"AttestationResult_reauth", AttestationResultMessage{Type: AttestationResultReauth}},
	}
	for _, tt := range tests {
		data, err := json.Marshal(tt.msg)
		if err != nil {
			t.Fatalf("%s: marshal: %v", tt.name, err)
		}
		var env map[string]interface{}
		if err := json.Unmarshal(data, &env); err != nil {
			t.Fatalf("%s: unmarshal envelope: %v", tt.name, err)
		}
		typ, ok := env["type"].(string)
		if !ok || typ == "" {
			t.Errorf("%s: missing or empty 'type' discriminator: %s", tt.name, data)
		}
	}
}

func TestBackendClaims_OutboundOmitEmpty(t *testing.T) {
	// When outbound is not set, the fields should be omitted from JSON.
	claims := BackendClaims{Weight: 1}
	data, _ := json.Marshal(claims)
	var raw map[string]interface{}
	_ = json.Unmarshal(data, &raw)
	if _, ok := raw["outbound_allowed"]; ok {
		t.Error("outbound_allowed should be omitted when false")
	}
	if _, ok := raw["allowed_outbound_ports"]; ok {
		t.Error("allowed_outbound_ports should be omitted when empty")
	}
}
