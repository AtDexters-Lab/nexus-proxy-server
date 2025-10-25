package routing

import (
	"log"
	"sync"

	hn "github.com/AtDexters-Lab/nexus-proxy-server/internal/hostnames"
	"github.com/AtDexters-Lab/nexus-proxy-server/internal/iface"
)

// Table holds the dynamic routing information for the mesh.
type Table struct {
	// routes maps a hostname (string) to a PeerList.
	routes sync.Map
	// wildcardRoutes maps a suffix like ".example.com" to a PeerList for single-label wildcard patterns.
	wildcardRoutes sync.Map
	// peerStates maps a peer's address (string) to the last known version
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

// UpdateRoutesForPeer updates the routing table with the hostnames serviced by a peer.
func (t *Table) UpdateRoutesForPeer(p iface.Peer, version uint64, hostnames []string) {
	rawState, _ := t.peerStates.LoadOrStore(p.Addr(), peerState{version: 0, hostnames: make(map[string]struct{})})
	state := rawState.(peerState)

	if version <= state.version {
		log.Printf("DEBUG: Ignoring stale announcement from peer %s (local version: %d, incoming: %d)", p.Addr(), state.version, version)
		return
	}

	newHostnameSet := make(map[string]struct{})
	for _, h := range hostnames {
		newHostnameSet[h] = struct{}{}
	}

	// Add new routes
	for hostname := range newHostnameSet {
		if _, ok := state.hostnames[hostname]; ok {
			continue
		}
		if suffix, ok := hn.WildcardSuffix(hostname); ok {
			rawList, _ := t.wildcardRoutes.LoadOrStore(suffix, NewPeerList())
			peerList := rawList.(*PeerList)
			peerList.Add(p)
		} else {
			rawList, _ := t.routes.LoadOrStore(hostname, NewPeerList())
			peerList := rawList.(*PeerList)
			peerList.Add(p)
		}
	}

	// Remove old routes
	for hostname := range state.hostnames {
		if _, ok := newHostnameSet[hostname]; ok {
			continue
		}
		if suffix, ok := hn.WildcardSuffix(hostname); ok {
			if rawList, ok := t.wildcardRoutes.Load(suffix); ok {
				peerList := rawList.(*PeerList)
				peerList.Remove(p)
			}
		} else {
			if rawList, ok := t.routes.Load(hostname); ok {
				peerList := rawList.(*PeerList)
				peerList.Remove(p)
			}
		}
	}

	t.peerStates.Store(p.Addr(), peerState{version: version, hostnames: newHostnameSet})
	log.Printf("DEBUG: Routing table updated for peer %s to version %d.", p.Addr(), version)
}

// ClearRoutesForPeer removes all routes associated with a given peer.
func (t *Table) ClearRoutesForPeer(p iface.Peer) {
	if rawState, ok := t.peerStates.LoadAndDelete(p.Addr()); ok {
		state := rawState.(peerState)
		for hostname := range state.hostnames {
			if suffix, ok := hn.WildcardSuffix(hostname); ok {
				if rawList, ok := t.wildcardRoutes.Load(suffix); ok {
					peerList := rawList.(*PeerList)
					peerList.Remove(p)
				}
				continue
			}
			if rawList, ok := t.routes.Load(hostname); ok {
				peerList := rawList.(*PeerList)
				peerList.Remove(p)
			}
		}
	}
}

// GetPeerForHostname finds a peer that services the given hostname.
func (t *Table) GetPeerForHostname(hostname string) (iface.Peer, bool) {
	if rawList, ok := t.routes.Load(hostname); ok {
		peerList := rawList.(*PeerList)
		if peer, err := peerList.Select(); err == nil {
			return peer, true
		}
		log.Println("DEBUG: No available peer for hostname", hostname)
	}
	// Try single-label wildcard based on first-dot suffix
	if suffix, ok := hn.FirstDotSuffix(hostname); ok {
		if rawList, ok := t.wildcardRoutes.Load(suffix); ok {
			peerList := rawList.(*PeerList)
			if peer, err := peerList.Select(); err == nil {
				return peer, true
			}
			log.Println("DEBUG: No available peer for wildcard suffix", suffix)
		}
	}
	return nil, false
}
