package client

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/transport"
	"fdb.dev/pkg/fdbgo/wire/types"
)

// TestCommit_RechecksInvalidAtomicPoison_SetDuringReadBarrier is the deterministic fault-injection
// regression for the round-10 poison re-check (concurrency test-debt #1). It parks Commit IN the read
// barrier (draining a pipelined read whose reply is dropped, so Resolve re-drives and blocks on the
// re-send's held reply), injects an invalid Atomic (setting the poison AFTER Commit's entry check but
// BEFORE the mutation snapshot), then releases — and asserts Commit returns invalid_mutation_type
// (2018). The entry poison check saw nil; only the re-read under the snapshot lock catches the racing
// poison. Revert-proof: without the re-read, this Commit succeeds despite the invalid atomic.
func TestCommit_RechecksInvalidAtomicPoison_SetDuringReadBarrier(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	db, sd := newSimTestDB(t, ctx)
	key := []byte(t.Name() + "_k")

	if _, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, []byte("v0"))
		return nil, nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rv, _, err := db.db.grvBatchers[grvBatcherDefault].getReadVersion(db.db, ctx, grvPriorityDefault, types.SpanContext{}, false, false)
	if err != nil {
		t.Fatalf("GRV: %v", err)
	}
	storageAddr := storageAddrFor(t, db, ctx, key)

	// 1st storage reply → DROP (the pipelined read stays pending → Commit's barrier Resolve times out
	// and re-drives). 2nd reply (the re-send from Resolve) → HOLD + signal: Commit is now parked in
	// the barrier, PAST the entry poison check but BEFORE the mutation snapshot.
	var firings int32
	parked := make(chan struct{}, 1)
	release := make(chan struct{})
	sd.setIntercept(func(_ int, _ transport.UID, body []byte) ([]byte, bool) {
		switch atomic.AddInt32(&firings, 1) {
		case 1:
			return nil, true // drop
		case 2:
			parked <- struct{}{}
			<-release
		}
		return body, false
	})
	sd.armAddr(storageAddr)

	tx := db.CreateTransaction()
	tx.SetReadVersion(rv)
	tx.rpcTimeoutOverride = 400 * time.Millisecond // short: Resolve re-drives quickly on the dropped reply

	if _, _, err := tx.GetPipelined(ctx, key); err != nil {
		t.Fatalf("GetPipelined: %v", err)
	}

	commitErr := make(chan error, 1)
	go func() { commitErr <- tx.Commit(ctx) }()

	select {
	case <-parked:
	case <-time.After(30 * time.Second):
		t.Fatal("Commit never re-drove the barrier read (did not park)")
	}

	// Poison the commit NOW — after the entry check, while Commit is parked in the barrier.
	tx.Atomic(MutClearRange, []byte(t.Name()+"_bad"), []byte("v")) // invalid op-code → poison
	close(release)                                                 // let the barrier read complete → Commit proceeds to the snapshot + re-check

	select {
	case err := <-commitErr:
		if fdbCodeOf(err) != ErrInvalidMutationType {
			t.Fatalf("Commit must catch the poison set during the barrier via the re-read → 2018, got %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Commit did not return after release")
	}
}
