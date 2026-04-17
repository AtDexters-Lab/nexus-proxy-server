package client

// Test-only accessors for creditLedger internal state. Files named
// *_test.go only compile under `go test`, so these are invisible to
// production builds.

func (l *creditLedger) ProbeFiredForTest() bool    { return l.probeFired.Load() }
func (l *creditLedger) ProbeGenForTest() int64     { return l.probeGen.Load() }
func (l *creditLedger) SignaledForTest() bool      { return l.signaled.Load() }
func (l *creditLedger) ClosedForTest() bool        { return l.closed.Load() }
func (l *creditLedger) InitializedForTest() bool   { return l.initialized.Load() }
func (l *creditLedger) UnlimitedForTest() bool     { return l.unlimited.Load() }
func (l *creditLedger) ForceSignaledForTest(v bool) { l.signaled.Store(v) }
func (l *creditLedger) SetAvailableForTest(n int64) { l.available.Store(n) }

// ResetStallCycleForTest simulates the stall-cycle side effect of
// Replenish(n>0) — clears probeFired and advances probeGen — so
// tests can force a post-replenishment state without running the
// full Replenish path.
func (l *creditLedger) ResetStallCycleForTest() { l.clearStallCycle() }
