package routing

import (
	"log"
	"sync"

	"github.com/AtDexters-Lab/nexus-proxy/internal/iface"
)

// Table holds the dynamic routing information for the mesh.
type Table struct {
	// routes maps a hostname (string) to a peer instance (Peer).
	routes sync.Map
	// peerRoutes maps a peer's address (string) to the last known version
	// and set of hostnames for that peer.
	peerStates sync.Map // map[peer.Addr()]peerState
}

type peerState struct {
	version   uint64
	hostnames map[string]struct{}
}

// NewTable creates a new routing table.
func NewTable() *Table {
	return &Table{}
}

// UpdateRoutesForPeer updates the routing table with the hostnames serviced by a peer,
// but only if the provided version number is newer than the last one seen.
func (t *Table) UpdateRoutesForPeer(p iface.Peer, version uint64, hostnames []string) {
	// Get the last known state for this peer.
	rawState, _ := t.peerStates.LoadOrStore(p.Addr(), peerState{version: 0, hostnames: make(map[string]struct{})})
	state := rawState.(peerState)

	// If the incoming version is not newer, ignore this update as it's stale.
	if version <= state.version {
		log.Printf("DEBUG: Ignoring stale announcement from peer %s (local version: %d, incoming: %d)", p.Addr(), state.version, version)
		return
	}

	// First, remove all old routes associated with this peer.
	for hostname := range state.hostnames {
		t.routes.Delete(hostname)
	}

	// Now, add the new routes from the fresh announcement.
	newHostnames := make(map[string]struct{})
	for _, h := range hostnames {
		t.routes.Store(h, p)
		newHostnames[h] = struct{}{}
	}

	// Store the new state (version and hostnames) for this peer.
	t.peerStates.Store(p.Addr(), peerState{version: version, hostnames: newHostnames})

	log.Printf("DEBUG: Routing table updated for peer %s to version %d. Total routes: %d", p.Addr(), version, t.Count())
}

// ClearRoutesForPeer removes all routes associated with a given peer.
func (t *Table) ClearRoutesForPeer(p iface.Peer) {
	if rawState, ok := t.peerStates.Load(p.Addr()); ok {
		state := rawState.(peerState)
		for hostname := range state.hostnames {
			t.routes.Delete(hostname)
		}
		t.peerStates.Delete(p.Addr())
	}
}

// GetPeerForHostname finds a peer that services the given hostname.
func (t *Table) GetPeerForHostname(hostname string) (iface.Peer, bool) {
	if rawPeer, ok := t.routes.Load(hostname); ok {
		if p, ok := rawPeer.(iface.Peer); ok {
			return p, true
		}
	}
	return nil, false
}

// Count returns the total number of unique routes in the table. (For debugging)
func (t *Table) Count() int {
	count := 0
	t.routes.Range(func(key, value interface{}) bool {
		count++
		return true
	})
	return count
}
