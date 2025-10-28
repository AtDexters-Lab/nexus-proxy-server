package hub_test

import (
	"log"
	"testing"

	"github.com/AtDexters-Lab/nexus-proxy-server/internal/config"
	"github.com/AtDexters-Lab/nexus-proxy-server/internal/hub"
)

func TestLoadBalancer(t *testing.T) {
	lb := hub.NewLoadBalancerPool()

	// Helper to create a Backend
	cfg := &config.Config{BackendsJWTSecret: "secret"}
	newBackend := func(host string, weight int) *hub.Backend {
		meta := &hub.AttestationMetadata{Hostnames: []string{host}, Weight: weight}
		return hub.NewBackend(nil, meta, cfg, stubValidator{})
	}

	// Test empty pool
	if lb.HasBackends() {
		t.Error("Expected HasBackends to be false for empty pool")
	}
	if _, err := lb.Select(); err == nil {
		t.Error("Expected error when selecting from empty pool")
	}

	// Add one backend
	b1 := newBackend("b1", 5)
	b1Id := b1.ID()
	lb.AddBackend(b1)
	if !lb.HasBackends() {
		t.Error("Expected HasBackends to be true after adding backend")
	}
	selected, err := lb.Select()
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}
	if selected != b1 {
		t.Error("Expected the only backend to be selected")
	}

	// Add more backends with different weights
	b2 := newBackend("b2", 1)
	b2Id := b2.ID()
	b3 := newBackend("b3", 4)
	b3Id := b3.ID()
	lb.AddBackend(b2)
	lb.AddBackend(b3)

	idbkNameMap := map[string]string{
		b1Id: "b1",
		b2Id: "b2",
		b3Id: "b3",
	}

	// Test weighted round robin distribution
	counts := map[string]int{}
	for i := 0; i < 1000; i++ {
		b, err := lb.Select()
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
		counts[idbkNameMap[b.(*hub.Backend).ID()]]++
	}
	// b1:5, b2:1, b3:4, so ratio should be about 5:1:4
	if counts["b1"] < 400 || counts["b1"] > 600 {
		t.Errorf("b1 selected %d times, expected ~500", counts["b1"])
	}
	if counts["b2"] < 50 || counts["b2"] > 150 {
		t.Errorf("b2 selected %d times, expected ~100", counts["b2"])
	}
	if counts["b3"] < 300 || counts["b3"] > 500 {
		t.Errorf("b3 selected %d times, expected ~400", counts["b3"])
	}
	log.Println("Selection counts:", counts)

	// Remove a backend and test selection
	lb.RemoveBackend(b1)
	counts = map[string]int{}
	for i := 0; i < 500; i++ {
		b, err := lb.Select()
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
		counts[idbkNameMap[b.(*hub.Backend).ID()]]++
	}
	if counts["b1"] != 0 {
		t.Errorf("b1 should not be selected after removal")
	}
	if counts["b2"] == 0 || counts["b3"] == 0 {
		t.Errorf("b2 and b3 should still be selected")
	}
	log.Println("Selection counts:", counts)

	// Remove all backends
	lb.RemoveBackend(b2)
	lb.RemoveBackend(b3)
	if lb.HasBackends() {
		t.Error("Expected HasBackends to be false after removing all backends")
	}
	if _, err := lb.Select(); err == nil {
		t.Error("Expected error when selecting from empty pool")
	}
}
