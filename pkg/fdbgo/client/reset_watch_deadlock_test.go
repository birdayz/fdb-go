package client

import (
	"context"
	"testing"
	"time"
)

// Reset must release a WatchSetup GRV before waiting for its execution lease.
func TestReset_DoesNotDeadlockWithWatchSetupGRV(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	db, gate, _ := newGRVReplyGateDB(t, ctx)
	unrelated := db.CreateTransaction()
	version, err := unrelated.GetReadVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	unrelated.SetReadVersion(version)
	gate.arm(1, false)
	tx := db.CreateTransaction()
	entry := make(chan struct{})
	releaseEntry, releaseEntryIt := releaseGate(t)
	tx.beforeReadVersionLock = func() { close(entry); <-releaseEntry }
	watchErr := make(chan error, 1)
	go func() { watchErr <- tx.Watch(ctx, []byte(t.Name()+"_k")) }()
	waitReadParked(t, ctx, entry, "watch before GRV entry")
	if _, err := unrelated.Get(ctx, []byte(t.Name()+"_unrelated")); err != nil {
		t.Fatalf("unrelated real read: %v", err)
	}
	select {
	case <-gate.heldCh:
		t.Fatal("unrelated real read signaled the watch GRV gate")
	default:
	}
	releaseEntryIt()
	waitReadParked(t, ctx, gate.heldCh, "actual watch GRV reply")
	tx.readVersionMu.Lock()
	pending := tx.pendingReadVersion != nil
	tx.readVersionMu.Unlock()
	if !pending {
		t.Fatal("watch has no pending GRV")
	}
	resetDone := make(chan struct{})
	go func() { tx.Reset(); close(resetDone) }()
	select {
	case <-resetDone:
	case <-ctx.Done():
		t.Fatal("Reset waited for a WatchSetup GRV that only its retirement can release")
	}
	gate.release()
	<-watchErr
	tx.beforeReadVersionLock = nil
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("post-Reset commit on the reset handle: %v", err)
	}
}
