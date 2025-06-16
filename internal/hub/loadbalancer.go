package hub

import (
	"fmt"
	"math"
	"sync"

	"github.com/AtDexters-Lab/nexus-proxy-server/internal/iface"
)

// LoadBalancerPool manages a collection of backend instances for a single hostname.
type LoadBalancerPool struct {
	backends          []*Backend
	mu                sync.RWMutex
	currentWeight     int
	currentIndex      int
	greatestCommonDiv int
}

// NewLoadBalancerPool creates a new, empty load balancer pool.
func NewLoadBalancerPool() *LoadBalancerPool {
	return &LoadBalancerPool{
		backends:          make([]*Backend, 0),
		currentIndex:      -1,
		currentWeight:     0,
		greatestCommonDiv: 1,
	}
}

func (p *LoadBalancerPool) AddBackend(b *Backend) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.backends = append(p.backends, b)
	p.recalculateGCD()
}

func (p *LoadBalancerPool) RemoveBackend(b *Backend) {
	p.mu.Lock()
	defer p.mu.Unlock()
	indexToRemove := -1
	for i, backend := range p.backends {
		if backend.id == b.id {
			indexToRemove = i
			break
		}
	}
	if indexToRemove != -1 {
		p.backends[indexToRemove] = p.backends[len(p.backends)-1]
		p.backends = p.backends[:len(p.backends)-1]
		p.recalculateGCD()
	}
}

// HasBackends returns true if there are one or more backends in the pool.
func (p *LoadBalancerPool) HasBackends() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.backends) > 0
}

// Select chooses a backend from the pool using the Weighted Round Robin algorithm.
// It returns an error if no backends are available.
func (p *LoadBalancerPool) Select() (iface.Backend, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.backends) == 0 {
		return nil, fmt.Errorf("no backends available in the pool")
	}
	if len(p.backends) == 1 {
		return p.backends[0], nil
	}
	for {
		p.currentIndex = (p.currentIndex + 1) % len(p.backends)
		if p.currentIndex == 0 {
			p.currentWeight = p.currentWeight - p.greatestCommonDiv
			if p.currentWeight <= 0 {
				p.currentWeight = p.maxWeight()
				if p.currentWeight == 0 {
					return nil, fmt.Errorf("all backends have zero weight")
				}
			}
		}
		if p.backends[p.currentIndex].weight >= p.currentWeight {
			return p.backends[p.currentIndex], nil
		}
	}
}

func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

func (p *LoadBalancerPool) maxWeight() int {
	max := 0
	for _, b := range p.backends {
		if b.weight > max {
			max = b.weight
		}
	}
	return max
}

func (p *LoadBalancerPool) recalculateGCD() {
	if len(p.backends) == 0 {
		p.greatestCommonDiv = 1
		return
	}
	if len(p.backends) == 1 {
		p.greatestCommonDiv = p.backends[0].weight
		return
	}
	newGcd := int(math.Abs(float64(p.backends[0].weight)))
	for i := 1; i < len(p.backends); i++ {
		newGcd = gcd(newGcd, int(math.Abs(float64(p.backends[i].weight))))
	}
	p.greatestCommonDiv = newGcd
}
