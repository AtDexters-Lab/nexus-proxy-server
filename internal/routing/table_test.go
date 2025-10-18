package routing

import (
	"github.com/stretchr/testify/require"
	"net"
	"testing"
)

type dummyPeer struct{ addr string }

func (d *dummyPeer) Addr() string                             { return d.addr }
func (d *dummyPeer) Send([]byte)                              {}
func (d *dummyPeer) StartTunnel(_ net.Conn, _ string, _ bool) {}

func TestWildcardRoutingBasic(t *testing.T) {
	tbl := NewTable()
	p := &dummyPeer{addr: "peer-1"}

	// Announce a wildcard route
	tbl.UpdateRoutesForPeer(p, 1, []string{"*.example.com"})

	// Should match exactly one additional label
	if peer, ok := tbl.GetPeerForHostname("a.example.com"); !ok || peer.Addr() != "peer-1" {
		t.Fatalf("expected wildcard route to match a.example.com")
	}
	// Should not match multi-labels
	if _, ok := tbl.GetPeerForHostname("a.b.example.com"); ok {
		t.Fatalf("did not expect wildcard to match multi-label host")
	}
	// Should not match bare domain
	if _, ok := tbl.GetPeerForHostname("example.com"); ok {
		t.Fatalf("did not expect wildcard to match bare domain")
	}
}

func TestWildcardVsExactPrecedence(t *testing.T) {
	tbl := NewTable()
	p1 := &dummyPeer{addr: "peer-wild"}
	p2 := &dummyPeer{addr: "peer-exact"}

	tbl.UpdateRoutesForPeer(p1, 1, []string{"*.example.com"})
	tbl.UpdateRoutesForPeer(p2, 2, []string{"app.example.com"})

	peer, ok := tbl.GetPeerForHostname("app.example.com")
	require.True(t, ok)
	require.Equal(t, "peer-exact", peer.Addr(), "exact should take precedence over wildcard")
}
