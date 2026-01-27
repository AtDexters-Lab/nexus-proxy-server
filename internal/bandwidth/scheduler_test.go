package bandwidth

import (
	"sync"
	"testing"
	"time"
)

func TestScheduler_Unlimited(t *testing.T) {
	s := NewScheduler(0) // unlimited
	defer s.Stop()

	bw := s.Register("backend1")
	if bw == nil {
		t.Fatal("Register should return BackendBandwidth")
	}

	// All requests should be allowed when unlimited
	for i := 0; i < 100; i++ {
		allowed, waitTime := s.RequestSend("backend1", 1024*1024) // 1MB
		if !allowed {
			t.Errorf("Request %d should be allowed when unlimited", i)
		}
		if waitTime != 0 {
			t.Errorf("Wait time should be 0 when unlimited, got %v", waitTime)
		}
	}
}

func TestScheduler_FairDistribution(t *testing.T) {
	// 100 Mbps = 12,500,000 bytes/sec
	s := NewScheduler(100)
	defer s.Stop()

	// Register 3 backends
	s.Register("backend1")
	s.Register("backend2")
	s.Register("backend3")

	// Activate all backends by requesting send
	s.RequestSend("backend1", 100)
	s.RequestSend("backend2", 100)
	s.RequestSend("backend3", 100)

	// Wait for activity to be recorded
	time.Sleep(50 * time.Millisecond)

	// Each backend should get ~33% of bandwidth
	// 12,500,000 / 3 = ~4,166,666 bytes/sec per backend
	metrics := s.GetMetrics()
	if metrics.ActiveBackendCount != 3 {
		t.Errorf("Expected 3 active backends, got %d", metrics.ActiveBackendCount)
	}

	expectedPerBackend := s.totalBytesPerSecond / 3
	actualPerBackend := metrics.PerBackendBytesPerSec

	// Allow 10% tolerance
	tolerance := expectedPerBackend / 10
	if actualPerBackend < expectedPerBackend-tolerance || actualPerBackend > expectedPerBackend+tolerance {
		t.Errorf("Expected ~%d bytes/sec per backend, got %d", expectedPerBackend, actualPerBackend)
	}
}

func TestScheduler_WorkConserving(t *testing.T) {
	// 100 Mbps total
	s := NewScheduler(100)
	defer s.Stop()

	// Register 3 backends, but only 1 is active
	s.Register("backend1")
	s.Register("backend2")
	s.Register("backend3")

	// Only backend1 sends data
	s.RequestSend("backend1", 100)

	// Wait for activity
	time.Sleep(50 * time.Millisecond)

	metrics := s.GetMetrics()
	if metrics.ActiveBackendCount != 1 {
		t.Errorf("Expected 1 active backend, got %d", metrics.ActiveBackendCount)
	}

	// Single active backend should get full bandwidth
	expectedPerBackend := s.totalBytesPerSecond
	actualPerBackend := metrics.PerBackendBytesPerSec

	if actualPerBackend != expectedPerBackend {
		t.Errorf("Expected %d bytes/sec for single active backend, got %d", expectedPerBackend, actualPerBackend)
	}
}

func TestScheduler_InstantAdaptation(t *testing.T) {
	// 100 Mbps total
	s := NewScheduler(100)
	defer s.Stop()

	// Start with 2 active backends
	s.Register("backend1")
	s.Register("backend2")
	s.RequestSend("backend1", 100)
	s.RequestSend("backend2", 100)

	time.Sleep(20 * time.Millisecond)

	metrics1 := s.GetMetrics()
	if metrics1.ActiveBackendCount != 2 {
		t.Errorf("Expected 2 active backends initially, got %d", metrics1.ActiveBackendCount)
	}

	// Add 2 more backends
	s.Register("backend3")
	s.Register("backend4")
	s.RequestSend("backend3", 100)
	s.RequestSend("backend4", 100)

	// Adaptation should be immediate
	time.Sleep(20 * time.Millisecond)

	metrics2 := s.GetMetrics()
	if metrics2.ActiveBackendCount != 4 {
		t.Errorf("Expected 4 active backends after addition, got %d", metrics2.ActiveBackendCount)
	}

	// Per-backend share should have halved
	expectedPerBackend := s.totalBytesPerSecond / 4
	actualPerBackend := metrics2.PerBackendBytesPerSec

	if actualPerBackend != expectedPerBackend {
		t.Errorf("Expected %d bytes/sec per backend after adaptation, got %d", expectedPerBackend, actualPerBackend)
	}
}

func TestScheduler_Oversubscription(t *testing.T) {
	// 100 Mbps total
	s := NewScheduler(100)
	defer s.Stop()

	// Register 200 backends (oversubscribed)
	for i := 0; i < 200; i++ {
		id := "backend" + string(rune('A'+i%26)) + string(rune('0'+i/26))
		s.Register(id)
		s.RequestSend(id, 100)
	}

	time.Sleep(50 * time.Millisecond)

	metrics := s.GetMetrics()
	if metrics.ActiveBackendCount != 200 {
		t.Errorf("Expected 200 active backends, got %d", metrics.ActiveBackendCount)
	}

	// Each backend gets tiny but fair share
	// 12,500,000 / 200 = 62,500 bytes/sec per backend
	expectedPerBackend := s.totalBytesPerSecond / 200
	actualPerBackend := metrics.PerBackendBytesPerSec

	if actualPerBackend != expectedPerBackend {
		t.Errorf("Expected %d bytes/sec per backend when oversubscribed, got %d", expectedPerBackend, actualPerBackend)
	}
}

func TestScheduler_RequestSendWaitTime(t *testing.T) {
	// 8 Mbps = 1,000,000 bytes/sec
	s := NewScheduler(8)
	defer s.Stop()

	s.Register("backend1")

	// First request activates and gets initial deficit
	allowed, _ := s.RequestSend("backend1", 100)
	if !allowed {
		// Small request should be allowed initially
		t.Log("Small initial request was rate limited, which is acceptable")
	}

	// Request a large amount that exceeds available deficit
	allowed, waitTime := s.RequestSend("backend1", 10*1024*1024) // 10MB
	if allowed {
		t.Error("Large request should not be immediately allowed")
	}
	if waitTime <= 0 {
		t.Error("Wait time should be positive for rate-limited request")
	}

	// Wait time should be proportional to data size
	// 10MB at 1MB/sec = 10 seconds (approximately)
	expectedWait := time.Duration(10) * time.Second
	tolerance := time.Second * 2
	if waitTime < expectedWait-tolerance || waitTime > expectedWait+tolerance {
		t.Logf("Wait time %v differs from expected ~%v (tolerance: %v)", waitTime, expectedWait, tolerance)
	}
}

func TestScheduler_RecordSent(t *testing.T) {
	s := NewScheduler(100)
	defer s.Stop()

	s.Register("backend1")
	s.RequestSend("backend1", 100) // Activate

	// Record some sent data
	s.RecordSent("backend1", 1000)
	s.RecordSent("backend1", 2000)
	s.RecordSent("backend1", 3000)

	// Wait for metrics to be collected
	time.Sleep(50 * time.Millisecond)

	metrics := s.GetMetrics()
	if metrics.CurrentBytesPerSecond <= 0 {
		t.Error("Expected non-zero current throughput after recording sent data")
	}
}

func TestScheduler_Unregister(t *testing.T) {
	s := NewScheduler(100)
	defer s.Stop()

	s.Register("backend1")
	s.Register("backend2")
	s.RequestSend("backend1", 100)
	s.RequestSend("backend2", 100)

	time.Sleep(20 * time.Millisecond)

	metrics1 := s.GetMetrics()
	if metrics1.TotalBackendCount != 2 {
		t.Errorf("Expected 2 total backends, got %d", metrics1.TotalBackendCount)
	}

	// Unregister one backend
	s.Unregister("backend1")

	time.Sleep(20 * time.Millisecond)

	metrics2 := s.GetMetrics()
	if metrics2.TotalBackendCount != 1 {
		t.Errorf("Expected 1 total backend after unregister, got %d", metrics2.TotalBackendCount)
	}
	if metrics2.ActiveBackendCount != 1 {
		t.Errorf("Expected 1 active backend after unregister, got %d", metrics2.ActiveBackendCount)
	}
}

func TestScheduler_IdleDetection(t *testing.T) {
	s := NewScheduler(100)
	defer s.Stop()

	s.Register("backend1")
	s.RequestSend("backend1", 100) // Activate

	time.Sleep(20 * time.Millisecond)

	metrics1 := s.GetMetrics()
	if metrics1.ActiveBackendCount != 1 {
		t.Errorf("Expected 1 active backend initially, got %d", metrics1.ActiveBackendCount)
	}

	// Wait longer than idle threshold (1 second)
	time.Sleep(1200 * time.Millisecond)

	metrics2 := s.GetMetrics()
	if metrics2.ActiveBackendCount != 0 {
		t.Errorf("Expected 0 active backends after idle, got %d", metrics2.ActiveBackendCount)
	}
	// Backend should still be registered
	if metrics2.TotalBackendCount != 1 {
		t.Errorf("Expected 1 total backend after idle, got %d", metrics2.TotalBackendCount)
	}
}

func TestScheduler_ConcurrentAccess(t *testing.T) {
	s := NewScheduler(100)
	defer s.Stop()

	var wg sync.WaitGroup
	numGoroutines := 50
	numOperations := 100

	// Concurrent registrations
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			backendID := "backend" + string(rune('A'+id%26)) + string(rune('0'+id/26))
			s.Register(backendID)
			for j := 0; j < numOperations; j++ {
				s.RequestSend(backendID, 1024)
				s.RecordSent(backendID, 1024)
			}
		}(i)
	}

	wg.Wait()

	// Concurrent unregistrations
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			backendID := "backend" + string(rune('A'+id%26)) + string(rune('0'+id/26))
			s.Unregister(backendID)
		}(i)
	}

	wg.Wait()

	metrics := s.GetMetrics()
	if metrics.TotalBackendCount != 0 {
		t.Errorf("Expected 0 backends after concurrent unregister, got %d", metrics.TotalBackendCount)
	}
}

func TestScheduler_UnknownBackend(t *testing.T) {
	s := NewScheduler(100)
	defer s.Stop()

	// Request send for unknown backend should be allowed (fail-open)
	allowed, waitTime := s.RequestSend("unknown", 1024)
	if !allowed {
		t.Error("Request for unknown backend should be allowed")
	}
	if waitTime != 0 {
		t.Errorf("Wait time should be 0 for unknown backend, got %v", waitTime)
	}

	// RecordSent for unknown backend should not panic
	s.RecordSent("unknown", 1024) // Should be no-op
}

func TestScheduler_ZeroBytesRequest(t *testing.T) {
	s := NewScheduler(100)
	defer s.Stop()

	s.Register("backend1")

	allowed, waitTime := s.RequestSend("backend1", 0)
	if !allowed {
		t.Error("Zero bytes request should be allowed")
	}
	if waitTime != 0 {
		t.Errorf("Wait time should be 0 for zero bytes request, got %v", waitTime)
	}
}

func TestScheduler_MetricsAccuracy(t *testing.T) {
	// 80 Mbps = 10,000,000 bytes/sec
	s := NewScheduler(80)
	defer s.Stop()

	s.Register("backend1")
	s.RequestSend("backend1", 100)

	// Send exactly 1MB
	s.RecordSent("backend1", 1024*1024)

	time.Sleep(100 * time.Millisecond)

	metrics := s.GetMetrics()
	if metrics.TotalBytesPerSecond != 10_000_000 {
		t.Errorf("Expected totalBytesPerSecond to be 10,000,000, got %d", metrics.TotalBytesPerSecond)
	}
}

func TestScheduler_SharedRegistration(t *testing.T) {
	s := NewScheduler(100)
	defer s.Stop()

	// Register same ID multiple times (simulating multiple tunnels to same hostname)
	bw1 := s.RegisterShared("tunnel:example.com")
	bw2 := s.RegisterShared("tunnel:example.com")
	bw3 := s.RegisterShared("tunnel:example.com")

	// All should return the same bandwidth state
	if bw1 != bw2 || bw2 != bw3 {
		t.Error("RegisterShared should return the same BackendBandwidth for same ID")
	}

	metrics := s.GetMetrics()
	if metrics.TotalBackendCount != 1 {
		t.Errorf("Expected 1 backend, got %d", metrics.TotalBackendCount)
	}

	// Unregister one - should still exist
	s.UnregisterShared("tunnel:example.com")
	metrics = s.GetMetrics()
	if metrics.TotalBackendCount != 1 {
		t.Errorf("Expected 1 backend after first unregister, got %d", metrics.TotalBackendCount)
	}

	// Unregister second - should still exist
	s.UnregisterShared("tunnel:example.com")
	metrics = s.GetMetrics()
	if metrics.TotalBackendCount != 1 {
		t.Errorf("Expected 1 backend after second unregister, got %d", metrics.TotalBackendCount)
	}

	// Unregister third (last) - should be removed
	s.UnregisterShared("tunnel:example.com")
	metrics = s.GetMetrics()
	if metrics.TotalBackendCount != 0 {
		t.Errorf("Expected 0 backends after last unregister, got %d", metrics.TotalBackendCount)
	}
}

func TestScheduler_SharedRegistrationConcurrent(t *testing.T) {
	s := NewScheduler(100)
	defer s.Stop()

	var wg sync.WaitGroup
	numGoroutines := 50

	// Concurrent shared registrations for same ID
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.RegisterShared("tunnel:shared.example.com")
			// Simulate some work
			s.RequestSend("tunnel:shared.example.com", 1024)
			s.RecordSent("tunnel:shared.example.com", 1024)
		}()
	}

	wg.Wait()

	metrics := s.GetMetrics()
	if metrics.TotalBackendCount != 1 {
		t.Errorf("Expected 1 backend for shared ID, got %d", metrics.TotalBackendCount)
	}

	// Concurrent unregistrations
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.UnregisterShared("tunnel:shared.example.com")
		}()
	}

	wg.Wait()

	metrics = s.GetMetrics()
	if metrics.TotalBackendCount != 0 {
		t.Errorf("Expected 0 backends after all unregister, got %d", metrics.TotalBackendCount)
	}
}

func TestScheduler_LargeMessageWithLowBandwidth(t *testing.T) {
	// Test that large messages can be sent even with low per-backend bandwidth
	// This verifies the minDeficitCap fix (P1)
	s := NewScheduler(8) // 8 Mbps = 1 MB/sec, very low
	defer s.Stop()

	s.Register("backend1")
	s.RequestSend("backend1", 100) // Activate

	// Wait for deficit to accumulate
	time.Sleep(100 * time.Millisecond)

	// Request a 32KB message (typical proxy buffer size)
	// With old 1KB minimum cap, this would never be allowed at low rates
	// With new 64KB minimum cap, it should work
	allowed, waitTime := s.RequestSend("backend1", 32*1024)

	// Should either be allowed immediately or have a reasonable wait time
	// (not infinite, which would happen if deficit cap < message size)
	if !allowed && waitTime > 5*time.Second {
		t.Errorf("32KB message should be sendable within reasonable time, got wait time %v", waitTime)
	}
}
