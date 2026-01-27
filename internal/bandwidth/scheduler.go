package bandwidth

import (
	"log"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// schedulingInterval defines how often deficit is replenished.
	schedulingInterval = 10 * time.Millisecond

	// maxDeficitMultiplier caps accumulated deficit to prevent burst after idle period.
	maxDeficitMultiplier = 2.0

	// idleThreshold is how long a backend must be inactive before being marked idle.
	idleThreshold = 1 * time.Second

	// metricsLogInterval is how often to log high utilization metrics.
	metricsLogInterval = 10 * time.Second

	// highUtilizationThreshold triggers logging when utilization exceeds this.
	highUtilizationThreshold = 0.80

	// minDeficitCap ensures the deficit cap is large enough to handle maximum message sizes.
	// The HTTP sniff path can forward up to 64KB of prelude, and SendData adds a 17-byte
	// protocol header, so the largest possible message is ~65553 bytes. We use 128KB to
	// provide ample headroom and ensure single messages can always be sent.
	minDeficitCap = 128 * 1024
)

// Scheduler implements Deficit Round Robin for fair bandwidth allocation.
type Scheduler struct {
	totalBytesPerSecond int64    // Configured limit (0 = unlimited)
	backends            sync.Map // backendID → *BackendBandwidth
	activeCount         atomic.Int64

	// Reference counting for shared backend IDs (e.g., tunnel:hostname)
	sharedMu  sync.Mutex
	refCounts map[string]int64

	// Metrics
	totalBytesSent atomic.Int64
	lastResetTime  atomic.Int64 // Unix nano

	// Background goroutine control
	stopCh   chan struct{}
	stopOnce sync.Once
}

// BackendBandwidth tracks per-backend bandwidth state.
type BackendBandwidth struct {
	deficit    atomic.Int64 // Accumulated send credit (bytes)
	bytesSent  atomic.Int64 // Total bytes sent (for metrics)
	lastActive atomic.Int64 // Last activity timestamp (Unix nano)
	isActive   atomic.Bool  // Currently has pending data
}

// Metrics contains current bandwidth utilization statistics.
type Metrics struct {
	TotalBytesPerSecond   int64   // Configured limit
	CurrentBytesPerSecond float64 // Actual throughput
	ActiveBackendCount    int64   // Backends with pending data
	TotalBackendCount     int64   // All registered backends
	UtilizationPercent    float64 // CurrentBytes / TotalBytes * 100
	PerBackendBytesPerSec int64   // Current fair share
}

// NewScheduler creates a bandwidth scheduler.
// totalMbps: total bandwidth in megabits per second (0 = unlimited)
func NewScheduler(totalMbps int) *Scheduler {
	var totalBytesPerSecond int64
	if totalMbps > 0 {
		totalBytesPerSecond = int64(totalMbps) * 1_000_000 / 8
	}

	s := &Scheduler{
		totalBytesPerSecond: totalBytesPerSecond,
		refCounts:           make(map[string]int64),
		stopCh:              make(chan struct{}),
	}
	s.lastResetTime.Store(time.Now().UnixNano())

	// Start background goroutine for idle detection and metrics logging
	if totalBytesPerSecond > 0 {
		go s.backgroundLoop()
	}

	return s
}

// Register adds a backend to the scheduler.
func (s *Scheduler) Register(backendID string) *BackendBandwidth {
	bw := &BackendBandwidth{}
	bw.lastActive.Store(time.Now().UnixNano())
	s.backends.Store(backendID, bw)
	return bw
}

// Unregister removes a backend from the scheduler.
func (s *Scheduler) Unregister(backendID string) {
	if raw, ok := s.backends.Load(backendID); ok {
		bw := raw.(*BackendBandwidth)
		if bw.isActive.CompareAndSwap(true, false) {
			s.activeCount.Add(-1)
		}
		s.backends.Delete(backendID)
	}
}

// RegisterShared registers a shared backend ID with reference counting.
// Multiple callers can register the same ID; the backend is only removed
// when all callers have called UnregisterShared.
// This is used for tunnel bandwidth tracking where multiple tunnels may
// share the same hostname.
func (s *Scheduler) RegisterShared(backendID string) *BackendBandwidth {
	s.sharedMu.Lock()
	s.refCounts[backendID]++
	isNew := s.refCounts[backendID] == 1
	s.sharedMu.Unlock()

	if !isNew {
		// Backend already exists, return it
		if raw, ok := s.backends.Load(backendID); ok {
			return raw.(*BackendBandwidth)
		}
	}

	// Create or get the backend bandwidth state
	bw := &BackendBandwidth{}
	bw.lastActive.Store(time.Now().UnixNano())
	actual, _ := s.backends.LoadOrStore(backendID, bw)
	return actual.(*BackendBandwidth)
}

// UnregisterShared decrements the reference count for a shared backend ID.
// The backend is only removed when the reference count reaches zero.
func (s *Scheduler) UnregisterShared(backendID string) {
	s.sharedMu.Lock()
	count, ok := s.refCounts[backendID]
	if !ok {
		s.sharedMu.Unlock()
		return
	}

	count--
	if count <= 0 {
		delete(s.refCounts, backendID)
		// Clean up the backend entry while still holding the lock to prevent
		// a concurrent RegisterShared from re-adding a refcount for a backend
		// that is about to be deleted.
		if raw, ok := s.backends.Load(backendID); ok {
			bw := raw.(*BackendBandwidth)
			if bw.isActive.CompareAndSwap(true, false) {
				s.activeCount.Add(-1)
			}
			s.backends.Delete(backendID)
		}
		s.sharedMu.Unlock()
	} else {
		s.refCounts[backendID] = count
		s.sharedMu.Unlock()
	}
}

// RequestSend checks if a backend can send data.
// Returns: (allowed bool, waitTime time.Duration)
// If allowed=false, caller should wait for waitTime before retrying.
func (s *Scheduler) RequestSend(backendID string, bytes int) (bool, time.Duration) {
	if s.totalBytesPerSecond == 0 {
		return true, 0 // unlimited
	}

	raw, ok := s.backends.Load(backendID)
	if !ok {
		return true, 0 // unknown backend, allow (shouldn't happen)
	}
	bw := raw.(*BackendBandwidth)

	// Mark as active if not already.
	// Re-check that the backend is still registered after incrementing activeCount
	// to prevent permanent drift if Unregister runs concurrently.
	if !bw.isActive.Load() {
		if bw.isActive.CompareAndSwap(false, true) {
			s.activeCount.Add(1)
			// If the backend was unregistered between Load and here, undo the increment.
			if _, still := s.backends.Load(backendID); !still {
				if bw.isActive.CompareAndSwap(true, false) {
					s.activeCount.Add(-1)
				}
				return true, 0
			}
		}
	}

	// Replenish deficit based on time elapsed
	s.replenishDeficit(bw)

	// Atomically check and reserve deficit using CAS to prevent concurrent
	// goroutines from observing the same deficit and all proceeding.
	needed := int64(bytes)
	for {
		currentDeficit := bw.deficit.Load()
		if currentDeficit >= needed {
			if bw.deficit.CompareAndSwap(currentDeficit, currentDeficit-needed) {
				return true, 0
			}
			continue // Another goroutine changed deficit, retry
		}

		// Not enough deficit — calculate wait time
		activeCount := s.activeCount.Load()
		if activeCount <= 0 {
			activeCount = 1
		}
		bytesPerSecondPerBackend := s.totalBytesPerSecond / activeCount
		if bytesPerSecondPerBackend <= 0 {
			bytesPerSecondPerBackend = 1
		}
		bytesNeeded := needed - currentDeficit
		waitNanos := (bytesNeeded * int64(time.Second)) / bytesPerSecondPerBackend

		if waitNanos <= 0 {
			waitNanos = int64(time.Millisecond) // Minimum 1ms to avoid busy-spin
		}
		return false, time.Duration(waitNanos)
	}
}

// RefundSend returns previously reserved deficit when a send fails (e.g., channel full).
// This prevents tokens from being permanently lost on dropped messages.
func (s *Scheduler) RefundSend(backendID string, bytes int) {
	if s.totalBytesPerSecond == 0 {
		return
	}
	raw, ok := s.backends.Load(backendID)
	if !ok {
		return
	}
	bw := raw.(*BackendBandwidth)
	bw.deficit.Add(int64(bytes))
}

// RecordSent records that data was successfully sent.
func (s *Scheduler) RecordSent(backendID string, bytes int) {
	if s.totalBytesPerSecond == 0 {
		return // unlimited, no tracking needed
	}

	raw, ok := s.backends.Load(backendID)
	if !ok {
		return
	}
	bw := raw.(*BackendBandwidth)

	// Deficit already reserved in RequestSend via CAS; only update metrics.
	bw.bytesSent.Add(int64(bytes))
	bw.lastActive.Store(time.Now().UnixNano())
	s.totalBytesSent.Add(int64(bytes))
}

// GetMetrics returns current bandwidth metrics.
func (s *Scheduler) GetMetrics() Metrics {
	now := time.Now().UnixNano()
	lastReset := s.lastResetTime.Load()
	elapsedSec := float64(now-lastReset) / float64(time.Second)
	if elapsedSec <= 0 {
		elapsedSec = 1
	}

	totalSent := s.totalBytesSent.Load()
	currentBps := float64(totalSent) / elapsedSec

	var totalCount int64
	s.backends.Range(func(key, value interface{}) bool {
		totalCount++
		return true
	})

	activeCount := s.activeCount.Load()
	var perBackendBps int64
	if activeCount > 0 && s.totalBytesPerSecond > 0 {
		perBackendBps = s.totalBytesPerSecond / activeCount
	}

	var utilization float64
	if s.totalBytesPerSecond > 0 {
		utilization = (currentBps / float64(s.totalBytesPerSecond)) * 100
	}

	return Metrics{
		TotalBytesPerSecond:   s.totalBytesPerSecond,
		CurrentBytesPerSecond: currentBps,
		ActiveBackendCount:    activeCount,
		TotalBackendCount:     totalCount,
		UtilizationPercent:    utilization,
		PerBackendBytesPerSec: perBackendBps,
	}
}

// Stop terminates the background goroutine.
func (s *Scheduler) Stop() {
	s.stopOnce.Do(func() {
		close(s.stopCh)
	})
}

func (s *Scheduler) replenishDeficit(bw *BackendBandwidth) {
	now := time.Now().UnixNano()
	last := bw.lastActive.Load()
	if last == 0 {
		bw.lastActive.Store(now)
		return
	}

	elapsed := now - last
	if elapsed <= 0 {
		return
	}

	// Update lastActive BEFORE calculating tokens to prevent double-crediting
	// if multiple goroutines call replenishDeficit concurrently.
	// Use CAS to ensure only one goroutine credits for this time period.
	if !bw.lastActive.CompareAndSwap(last, now) {
		// Another goroutine already updated lastActive, skip replenishment
		return
	}

	activeCount := s.activeCount.Load()
	if activeCount <= 0 {
		activeCount = 1
	}

	// Calculate quantum for this backend
	bytesPerSecond := s.totalBytesPerSecond / activeCount
	if bytesPerSecond <= 0 {
		bytesPerSecond = 1
	}
	// Cap deficit to prevent burst, but ensure it's large enough for max message size
	maxDeficit := int64(float64(bytesPerSecond) * maxDeficitMultiplier * float64(schedulingInterval) / float64(time.Second))
	if maxDeficit < minDeficitCap {
		maxDeficit = minDeficitCap
	}

	// Compute bytes to add using division-first to prevent int64 overflow.
	// elapsed is in nanoseconds; dividing first avoids bytesPerSecond * elapsed overflow.
	elapsedSec := elapsed / int64(time.Second)
	elapsedFrac := elapsed % int64(time.Second)
	bytesToAdd := bytesPerSecond*elapsedSec + (bytesPerSecond*elapsedFrac)/int64(time.Second)
	// Clamp to maxDeficit since deficit is capped anyway
	if bytesToAdd > maxDeficit {
		bytesToAdd = maxDeficit
	}

	for {
		current := bw.deficit.Load()
		newDeficit := current + bytesToAdd
		if newDeficit > maxDeficit {
			newDeficit = maxDeficit
		}
		if bw.deficit.CompareAndSwap(current, newDeficit) {
			break
		}
	}
}

func (s *Scheduler) backgroundLoop() {
	idleTicker := time.NewTicker(idleThreshold)
	metricsTicker := time.NewTicker(metricsLogInterval)
	metricsResetTicker := time.NewTicker(time.Second)
	defer idleTicker.Stop()
	defer metricsTicker.Stop()
	defer metricsResetTicker.Stop()

	for {
		select {
		case <-s.stopCh:
			return
		case <-idleTicker.C:
			s.checkIdleBackends()
		case <-metricsTicker.C:
			s.logMetricsIfNeeded()
		case <-metricsResetTicker.C:
			s.resetMetrics()
		}
	}
}

func (s *Scheduler) checkIdleBackends() {
	now := time.Now().UnixNano()
	threshold := now - int64(idleThreshold)

	s.backends.Range(func(key, value interface{}) bool {
		bw := value.(*BackendBandwidth)
		if bw.isActive.Load() && bw.lastActive.Load() < threshold {
			if bw.isActive.CompareAndSwap(true, false) {
				s.activeCount.Add(-1)
				bw.deficit.Store(0) // Reset deficit on idle
			}
		}
		return true
	})
}

func (s *Scheduler) logMetricsIfNeeded() {
	metrics := s.GetMetrics()
	if metrics.UtilizationPercent >= highUtilizationThreshold*100 {
		currentMbps := metrics.CurrentBytesPerSecond * 8 / 1_000_000
		totalMbps := float64(metrics.TotalBytesPerSecond) * 8 / 1_000_000
		perBackendMbps := float64(metrics.PerBackendBytesPerSec) * 8 / 1_000_000

		log.Printf("INFO: Bandwidth: %.1f%% utilization (%.2f Mbps / %.0f Mbps), %d active backends, %.2f Mbps each",
			metrics.UtilizationPercent,
			currentMbps,
			totalMbps,
			metrics.ActiveBackendCount,
			perBackendMbps)

		if metrics.UtilizationPercent >= 95 {
			log.Printf("WARN: Bandwidth: 95%% utilization, consider increasing totalBandwidthMbps")
		}
	}
}

func (s *Scheduler) resetMetrics() {
	s.totalBytesSent.Store(0)
	s.lastResetTime.Store(time.Now().UnixNano())
}
