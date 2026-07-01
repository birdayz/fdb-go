package client

import (
	"sync"
	"testing"
)

// TestAtomic_InvalidOpPoison_RaceFree pins the atomic.Pointer synchronization of invalidAtomicOpErr
// (codex P2): Atomic() is a concurrent-safe data op that writes the poison (via CompareAndSwap)
// while Commit reads it at entry. Concurrent write||read must be `-race` clean. MUST be run under
// -race to catch a regression — reverting the field to a plain `error` makes this a data race.
func TestAtomic_InvalidOpPoison_RaceFree(t *testing.T) {
	t.Parallel()
	for i := 0; i < 200; i++ {
		tx := newTestTx()
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); tx.Atomic(MutClearRange, []byte("k"), []byte("v")) }() // CAS-writes the poison
		go func() { defer wg.Done(); _ = tx.invalidAtomicOpErr.Load() }()                   // Commit-entry read
		wg.Wait()
	}
}
