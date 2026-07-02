package client

import (
	"context"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/transport"
)

// TestReset_DoesNotDeadlockWithWatchSetupGRV pins the Reset-vs-WatchSetup lock order
// (RFC-175 E1 fold): WatchSetup's ensureReadVersion HOLDS readVersionMu across its GRV,
// and that GRV runs on the watch context that only reset()'s cancelWatches releases. If
// Reset() takes readVersionMu BEFORE running reset() (the metricStart clear), it blocks
// behind the parked watch goroutine that only Reset itself can unblock — a deadlock
// (transactions have no default timeout). Reset must run reset(true) — whose
// cancelWatches cancels the watch context, aborting the GRV and freeing the mutex —
// before it touches readVersionMu. C++ cannot hit this: reset and the watch share the
// network thread, and resetPromise fires before any state is rebuilt.
//
// Deterministic: the sim dialer HOLDS every reply frame (the watch's GRV can never
// complete on its own), and the test proceeds only once readVersionMu is observably
// held by the parked watch goroutine. Revert-proof: moving the readVersionMu-guarded
// metricStart clear back above tx.reset(true) in Reset() makes this test time out red.
func TestReset_DoesNotDeadlockWithWatchSetupGRV(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	db, sd := newSimTestDB(t, ctx)

	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	// Hold EVERY reply on every conn until release: the watch's GRV reply parks its
	// goroutine inside ensureReadVersion, under readVersionMu. Pass-through after release.
	sd.setIntercept(func(_ int, _ transport.UID, body []byte) ([]byte, bool) {
		select {
		case <-release:
			return body, false
		default:
		}
		<-release
		return body, false
	})
	sd.armAll()

	// No SetReadVersion: WatchSetup's ensureReadVersion must fetch a GRV (which the
	// intercept holds) while holding readVersionMu.
	tx := db.CreateTransaction()
	watchErr := make(chan error, 1)
	go func() { watchErr <- tx.Watch(ctx, []byte(t.Name()+"_k")) }()

	// Proceed only once the watch goroutine observably HOLDS readVersionMu (it is the
	// only user of this transaction, and once inside ensureReadVersion's critical
	// section it cannot leave until its GRV completes — which the intercept forbids —
	// or its watch context is cancelled, which only Reset/Cancel can do).
	held := false
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); {
		if tx.readVersionMu.TryLock() {
			tx.readVersionMu.Unlock()
			time.Sleep(2 * time.Millisecond)
			continue
		}
		held = true
		break
	}
	if !held {
		close(release)
		t.Fatal("watch goroutine never blocked inside ensureReadVersion holding readVersionMu")
	}

	// Reset must complete promptly: its reset(true) → cancelWatches cancels the watch
	// context, aborting the parked GRV and releasing readVersionMu.
	resetDone := make(chan struct{})
	go func() { tx.Reset(); close(resetDone) }()
	select {
	case <-resetDone:
	case <-time.After(20 * time.Second):
		t.Fatal("Reset deadlocked: it waited on readVersionMu held by a WatchSetup GRV that only Reset's own cancelWatches can release")
	}

	close(release)
	<-watchErr // drain; the cancelled watch's error shape is pinned elsewhere (divergence D5)

	// The handle is usable after the Reset (fresh conns flow post-release).
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("post-Reset commit on the reset handle: %v", err)
	}
}
