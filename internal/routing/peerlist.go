package routing

import (
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/AtDexters-Lab/nexus-proxy-server/internal/iface"
)

// PeerList manages a list of peers for a single hostname and provides
// simple round-robin selection.
type PeerList struct {
	peers   []iface.Peer
	mu      sync.RWMutex
	counter uint64
}

// NewPeerList creates a new peer list.
func NewPeerList() *PeerList {
	return &PeerList{
		peers: make([]iface.Peer, 0, 2),
	}
}

// Add adds a peer to the list if it's not already present.
func (pl *PeerList) Add(p iface.Peer) {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	for _, existingPeer := range pl.peers {
		if existingPeer.Addr() == p.Addr() {
			return // Already exists
		}
	}
	pl.peers = append(pl.peers, p)
}

// Remove removes a peer from the list.
func (pl *PeerList) Remove(p iface.Peer) {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	for i, existingPeer := range pl.peers {
		if existingPeer.Addr() == p.Addr() {
			// Remove from slice without preserving order
			pl.peers[i] = pl.peers[len(pl.peers)-1]
			pl.peers = pl.peers[:len(pl.peers)-1]
			return
		}
	}
}

// IsEmpty reports whether the peer list has any entries.
func (pl *PeerList) IsEmpty() bool {
	pl.mu.RLock()
	defer pl.mu.RUnlock()
	return len(pl.peers) == 0
}

// Select chooses a peer from the list using simple round-robin.
func (pl *PeerList) Select() (iface.Peer, error) {
	pl.mu.RLock()
	defer pl.mu.RUnlock()

	if len(pl.peers) == 0 {
		return nil, fmt.Errorf("no peers in list")
	}

	// Atomically increment the counter and get the next index.
	idx := atomic.AddUint64(&pl.counter, 1) % uint64(len(pl.peers))
	return pl.peers[idx], nil
}
