package client

import (
	"context"
	"testing"
)

// Not parallel: AllocsPerRun counts process-wide allocations.
func TestNativeCommitCallBackstopAllocations(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	n := &nativeCommit{ctx: ctx}
	input := &commitInput{db: &database{}}
	allocs := testing.AllocsPerRun(100, func() {
		if _, err, panicked := n.call(context.Background(), input); err == nil || panicked != nil {
			t.Fatalf("cancelled native commit = (%v, %v), want an error and no panic", err, panicked)
		}
	})
	if allocs != 0 {
		t.Fatalf("ALLOC_NATIVE_COMMIT_BACKSTOP: %v allocations per clean call, want 0", allocs)
	}

	// A panic still reaches the backstop and is reported to the caller.
	db := &database{}
	boom := func() { panic("boom") }
	db.beforeCommitProxySelect.Store(&boom)
	if _, _, panicked := n.call(context.Background(), &commitInput{db: db}); panicked != "boom" {
		t.Fatalf("panicking native commit reported %v, want boom", panicked)
	}
	if got := db.metrics.Snapshot().RecoveredPanics; got != 1 {
		t.Fatalf("recovered panics = %d, want 1", got)
	}
}
