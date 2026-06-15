package client

import (
	"testing"
)

// TestGRVFlush_RecoverFailsBatch is the RFC-110 Class A-batch contract: a panic
// in flush must FAIL the popped batch — every waiter gets an error, none hangs
// (codex P1) — AND must not leave b.mu locked (codex P2a), or a later GRV request
// blocking on b.mu.Lock() would deadlock. Drives the exact production shape: a
// panic while holding b.mu inside a closure-scoped lock, with recoverFlush
// deferred.
func TestGRVFlush_RecoverFailsBatch(t *testing.T) {
	t.Parallel()
	var db database
	b := &grvBatcher{}

	const n = 4
	batch := make([]grvRequest, n)
	for i := range batch {
		batch[i] = grvRequest{reply: make(chan grvResult, 1)}
	}

	func() {
		defer b.recoverFlush(&db, batch)
		// The adaptive-window region's closure-scoped lock: a panic here must
		// unwind b.mu (the deferred unlock), then recoverFlush fails the batch.
		func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			panic("boom while holding b.mu")
		}()
	}()

	// Mutex released — a later flush would block forever otherwise.
	if !b.mu.TryLock() {
		t.Fatal("b.mu left locked after a panic in the locked region (deadlock for future GRV requests)")
	}
	b.mu.Unlock()

	// Every waiter received an error result — none hangs (codex P1).
	for i := range batch {
		select {
		case res := <-batch[i].reply:
			if res.err == nil {
				t.Fatalf("waiter %d: nil err, want a failed-batch error", i)
			}
		default:
			t.Fatalf("waiter %d: no result delivered — a non-canceling caller would hang forever", i)
		}
	}

	if got := db.metrics.Snapshot().RecoveredPanics; got != 1 {
		t.Fatalf("RecoveredPanics = %d, want 1", got)
	}
}

// TestGRVFlush_NoPanicNoDeliver: on the normal path recover() is nil, so
// recoverFlush must be a no-op — it must NOT deliver to the batch (the normal
// flush path owns delivery; a double-send would block the next real delivery).
func TestGRVFlush_NoPanicNoDeliver(t *testing.T) {
	t.Parallel()
	var db database
	b := &grvBatcher{}
	batch := []grvRequest{{reply: make(chan grvResult, 1)}}

	func() {
		defer b.recoverFlush(&db, batch)
		// no panic
	}()

	select {
	case <-batch[0].reply:
		t.Fatal("recoverFlush delivered on the no-panic path")
	default:
	}
	if got := db.metrics.Snapshot().RecoveredPanics; got != 0 {
		t.Fatalf("RecoveredPanics = %d, want 0 on the no-panic path", got)
	}
}
