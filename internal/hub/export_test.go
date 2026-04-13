package hub

// Test-only accessors for bufferedConn internal counters.
// Production code never references these.

// InnerGateReplenishObservedCount returns the number of times the
// replenish retry timer fired and was observed by drain's INNER
// gate-wait select branch. Used by
// TestBufferedConn_GateDoesNotBlockReplenishRetry to assert that the
// inner-loop credit-retry path is actually exercised under bandwidth
// pressure (the v0.3.9 credit-loss fix's regression guard).
func (bc *bufferedConn) InnerGateReplenishObservedCount() int64 {
	return bc.innerGateReplenishObserved.Load()
}
