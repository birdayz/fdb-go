package client

import (
	"testing"
	"time"
)

func TestFailureMonitorBasic(t *testing.T) {
	t.Parallel()
	fm := newFailureMonitor()

	if fm.isFailed("10.0.0.1:4500") {
		t.Fatal("fresh monitor should not have any failed endpoints")
	}

	fm.markFailed("10.0.0.1:4500")
	if !fm.isFailed("10.0.0.1:4500") {
		t.Fatal("endpoint should be failed after markFailed")
	}

	fm.markAlive("10.0.0.1:4500")
	if fm.isFailed("10.0.0.1:4500") {
		t.Fatal("endpoint should be alive after markAlive")
	}
}

func TestFailureMonitorRecoverySignal(t *testing.T) {
	t.Parallel()
	fm := newFailureMonitor()

	fm.markFailed("10.0.0.1:4500")
	ch := fm.waitForRecovery()

	// Channel should not be closed yet.
	select {
	case <-ch:
		t.Fatal("recovered channel should not be closed before markAlive")
	case <-time.After(100 * time.Millisecond):
	}

	fm.markAlive("10.0.0.1:4500")

	// Channel should now be closed.
	select {
	case <-ch:
		// ok
	case <-time.After(100 * time.Millisecond):
		t.Fatal("recovered channel should be closed after failed→alive transition")
	}
}

func TestFailureMonitorNoSpuriousSignal(t *testing.T) {
	t.Parallel()
	fm := newFailureMonitor()

	ch := fm.waitForRecovery()

	// markAlive on an address that was never failed should not signal.
	fm.markAlive("10.0.0.1:4500")

	select {
	case <-ch:
		t.Fatal("recovered channel should not close for markAlive on unknown addr")
	case <-time.After(100 * time.Millisecond):
		// ok — no spurious signal
	}

	// Also: markAlive on an already-alive address should not signal.
	fm.markFailed("10.0.0.2:4500")
	fm.markAlive("10.0.0.2:4500")
	// Get a fresh channel after the first recovery.
	ch2 := fm.waitForRecovery()

	fm.markAlive("10.0.0.2:4500") // already alive — no transition
	select {
	case <-ch2:
		t.Fatal("recovered channel should not close for redundant markAlive")
	case <-time.After(100 * time.Millisecond):
		// ok
	}
}

func TestFailureMonitorMultipleRecoveries(t *testing.T) {
	t.Parallel()
	fm := newFailureMonitor()

	// First cycle.
	fm.markFailed("10.0.0.1:4500")
	ch1 := fm.waitForRecovery()
	fm.markAlive("10.0.0.1:4500")
	select {
	case <-ch1:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("first recovery should signal")
	}

	// Second cycle — fresh channel.
	ch2 := fm.waitForRecovery()
	select {
	case <-ch2:
		t.Fatal("fresh channel after recovery should not be closed")
	case <-time.After(100 * time.Millisecond):
	}

	fm.markFailed("10.0.0.1:4500")
	fm.markAlive("10.0.0.1:4500")
	select {
	case <-ch2:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("second recovery should signal on fresh channel")
	}
}

func TestFailureMonitorMultiEndpoint(t *testing.T) {
	t.Parallel()
	fm := newFailureMonitor()

	fm.markFailed("10.0.0.1:4500")
	fm.markFailed("10.0.0.2:4500")
	fm.markFailed("10.0.0.3:4500")

	ch := fm.waitForRecovery()

	// Recovering just one endpoint should wake waiters.
	fm.markAlive("10.0.0.2:4500")

	select {
	case <-ch:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("recovering 1 of 3 failed endpoints should signal")
	}

	// Other two should still be failed.
	if !fm.isFailed("10.0.0.1:4500") {
		t.Fatal("10.0.0.1 should still be failed")
	}
	if fm.isFailed("10.0.0.2:4500") {
		t.Fatal("10.0.0.2 should be alive")
	}
	if !fm.isFailed("10.0.0.3:4500") {
		t.Fatal("10.0.0.3 should still be failed")
	}
}
