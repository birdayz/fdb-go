package client

import (
	"context"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/transport"
)

// Reset must release a WatchSetup GRV before waiting for its execution lease.
func TestReset_DoesNotDeadlockWithWatchSetupGRV(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	db, sd := newSimTestDB(t, ctx)
	parked := make(chan struct{}, 1)
	release, releaseIt := releaseGate(t)
	sd.setIntercept(func(_ int, _ transport.UID, body []byte) ([]byte, bool) {
		select {
		case parked <- struct{}{}:
		default:
		}
		<-release
		return body, false
	})
	sd.armAll()
	tx := db.CreateTransaction()
	watchErr := make(chan error, 1)
	go func() { watchErr <- tx.Watch(ctx, []byte(t.Name()+"_k")) }()
	waitReadParked(t, ctx, parked, "watch GRV")
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
	releaseIt()
	<-watchErr
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("post-Reset commit on the reset handle: %v", err)
	}
}
