package client

import (
	"testing"
)

// TestGRVFlush_RecoverFailsBatch is the RFC-110 Class A-batch contract: a panic
// in flush must FAIL the popped batch — every waiter gets an error, none hangs.
// recoverFlush is the production backstop and is exercised directly
// here (deferred around a panic, exactly as flush defers it).
//
// It also demonstrates the closure-scoped-lock PATTERN flush uses for codex P2a:
// a panic inside a `Lock(); defer Unlock()` closure unwinds the mutex. NOTE this
// is the pattern, not flush's own locked lines — flush's two b.mu regions
// (pop + adaptive-window arithmetic, grv.go) contain no code that can panic, so
// the deadlock is a defense-in-depth guarantee for future edits, asserted here at
// the pattern level rather than by driving flush (which would need a real GRV
// round / network to reach those lines).
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
		// Same closure-scoped-lock pattern as flush's regions: a panic must unwind
		// b.mu via the deferred unlock before recoverFlush runs.
		func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			panic("boom while holding b.mu")
		}()
	}()

	// The closure-scoped unlock released b.mu through the panic (the pattern that
	// keeps a future panic in flush's locked regions from deadlocking GRV).
	if !b.mu.TryLock() {
		t.Fatal("closure-scoped lock did not release b.mu on panic")
	}
	b.mu.Unlock()

	// Every waiter received an error result — none hangs.
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
