package client

import "sync"

// failureMonitor tracks per-endpoint health state. When an endpoint
// transitions from failed to alive, all waiters are woken up via the
// recovered channel. This replaces C++'s quorum(ok,1) event-driven pattern.
type failureMonitor struct {
	mu        sync.Mutex
	failed    map[string]bool
	recovered chan struct{}
}

func newFailureMonitor() *failureMonitor {
	return &failureMonitor{
		failed:    make(map[string]bool),
		recovered: make(chan struct{}),
	}
}

// markFailed records that an endpoint is unhealthy. Returns true only on the
// alive→failed transition (the first failure of an episode), so a caller can
// edge-trigger a log without storming on every retry that re-hits a dead peer —
// the symmetric counterpart to markAlive's transition-only signal.
func (fm *failureMonitor) markFailed(addr string) (newlyFailed bool) {
	fm.mu.Lock()
	defer fm.mu.Unlock()
	if fm.failed[addr] {
		return false
	}
	fm.failed[addr] = true
	return true
}

// markAlive records that an endpoint is healthy. If the endpoint was
// previously failed, closes the recovered channel to wake all waiters,
// then creates a fresh channel for the next recovery cycle.
func (fm *failureMonitor) markAlive(addr string) {
	fm.mu.Lock()
	defer fm.mu.Unlock()
	if !fm.failed[addr] {
		return // no transition — don't signal
	}
	delete(fm.failed, addr)
	close(fm.recovered)
	fm.recovered = make(chan struct{})
}

// waitForRecovery returns a channel that is closed when any endpoint
// transitions from failed to alive. Each recovery cycle gets a fresh channel.
func (fm *failureMonitor) waitForRecovery() <-chan struct{} {
	fm.mu.Lock()
	defer fm.mu.Unlock()
	return fm.recovered
}

// isFailed returns whether an endpoint is currently marked as failed.
// Used by tests only.
func (fm *failureMonitor) isFailed(addr string) bool {
	fm.mu.Lock()
	defer fm.mu.Unlock()
	return fm.failed[addr]
}
