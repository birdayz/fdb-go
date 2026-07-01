package client

import (
	"context"
	"sync"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/transport"
	"fdb.dev/pkg/fdbgo/wire/types"
)

// TestWatchSetup_CancelDuringValueRead_ReleasesSlot is the deterministic fault-injection regression
// for the round-12 newWatchCtx-early fix (RFC follow-up / handover test-debt #2). It holds the
// WatchSetup value-read reply via the simDialer intercept so setup parks IN the read, cancels the
// transaction mid-read, then releases — and asserts the outstanding-watch slot is freed.
//
// With newWatchCtx bound BEFORE the read (the fix), the Cancel cancels the very context WatchPoll
// holds, so WatchPoll drains and releaseWatch runs → a second watch under MAX_WATCHES=1 succeeds.
// Revert-proof: with newWatchCtx bound AFTER the read, the Cancel's cancelWatches is a no-op (no
// context yet), the read completes, WatchSetup mints a FRESH never-cancelled context, and WatchPoll
// long-polls forever HOLDING the slot — so tx.Watch never returns (the <-watchErr wait times out) and
// the slot is never freed.
func TestWatchSetup_CancelDuringValueRead_ReleasesSlot(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	db, sd := newSimTestDB(t, ctx)
	key := []byte(t.Name() + "_k")

	// Seed the watched key (a DIFFERENT txn, so the watch txn's RYW get reads through to storage).
	if _, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, []byte("v0"))
		return nil, nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Warm the location cache + a read version so only the value-read frame flows in the armed window.
	rv, _, err := db.db.grvBatchers[grvBatcherDefault].getReadVersion(db.db, ctx, grvPriorityDefault, types.SpanContext{}, false, false)
	if err != nil {
		t.Fatalf("GRV: %v", err)
	}
	storageAddr := storageAddrFor(t, db, ctx, key)

	if err := db.SetMaxWatches(1); err != nil { // cap 1: a leaked slot makes the 2nd watch fail 1032
		t.Fatalf("SetMaxWatches: %v", err)
	}

	// Hold the FIRST storage reply (the watch value read) until released; signal when it parks.
	parked := make(chan struct{}, 1)
	release := make(chan struct{})
	sd.setIntercept(func(_ int, _ transport.UID, body []byte) ([]byte, bool) {
		select {
		case parked <- struct{}{}:
			<-release
		default:
		}
		return body, false
	})
	sd.armAddr(storageAddr)

	watchTx := db.CreateTransaction()
	watchTx.SetReadVersion(rv)
	watchErr := make(chan error, 1)
	go func() { watchErr <- watchTx.Watch(ctx, key) }() // WatchSetup (acquire + bind ctx) → value read parks

	select {
	case <-parked:
	case <-time.After(30 * time.Second):
		t.Fatal("WatchSetup never reached the value read")
	}

	watchTx.Cancel() // cancels the early-bound watchCtx
	close(release)   // let the value read complete → WatchPoll drains on the cancelled ctx → releaseWatch

	select {
	case <-watchErr: // the watch drained (fix); without the fix it long-polls forever and this times out
	case <-time.After(30 * time.Second):
		t.Fatal("Watch did not drain after Cancel — the slot leaked (newWatchCtx bound too late)")
	}

	// The slot must be free now — a fresh WatchSetup under cap=1 must not fail too_many_watches.
	sd.setIntercept(nil) // stop holding replies
	tx2 := db.CreateTransaction()
	tx2.SetReadVersion(rv)
	if _, _, _, _, _, err := tx2.WatchSetup(ctx, []byte(t.Name()+"_k2")); err != nil {
		t.Fatalf("cancelled watch must release its slot; fresh WatchSetup must succeed, got %v", err)
	}
	db.db.releaseWatch() // free tx2's slot (no WatchPoll runs for it)
}

// TestWatchSetup_CancelUnblocksStuckSetupRead is the deterministic regression for codex's round-19
// finding: WatchSetup's blocking setup reads must run on watchCtx (not the caller ctx), so a Cancel()
// during a PERMANENTLY-stuck value read unblocks the read and releases the reserved slot AT ONCE —
// matching C++'s watch actor `catch{ decreaseWatchCounter(); throw; }` on cancellation
// (NativeAPI.actor.cpp:5637-5682). Unlike TestWatchSetup_CancelDuringValueRead_ReleasesSlot (which
// RELEASES the held reply after cancelling, so the read completes on its own and masks the bug), this
// NEVER releases the reply until AFTER asserting Watch returned — proving it was the cancellation, not
// the reply, that unblocked the read. Revert-proof: with the reads on the caller ctx (the bug),
// cancelWatches cancels watchCtx but the read stays parked on the live caller ctx, so Watch hangs past
// the 20s inner deadline and the slot stays charged — the assertions below fail. The error must be
// transaction_cancelled (1025): a txn Cancel out-ranks the raw ctx cancellation (watchSetupErr).
func TestWatchSetup_CancelUnblocksStuckSetupRead(t *testing.T) {
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

	if err := db.SetMaxWatches(1); err != nil { // cap 1: a leaked slot makes the 2nd watch fail 1032
		t.Fatalf("SetMaxWatches: %v", err)
	}

	// Hold the value-read reply FOREVER (release is not closed until after the assertions). Guard the
	// close with a Once + defer so a FAILED assertion (the revert case) still unblocks the connection
	// read loop on the way out — otherwise the wedged loop would hang test cleanup instead of failing.
	parked := make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseIt := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseIt()
	sd.setIntercept(func(_ int, _ transport.UID, body []byte) ([]byte, bool) {
		select {
		case parked <- struct{}{}:
			<-release
		default:
		}
		return body, false
	})
	sd.armAddr(storageAddr)

	watchTx := db.CreateTransaction()
	watchTx.SetReadVersion(rv)
	watchErr := make(chan error, 1)
	go func() { watchErr <- watchTx.Watch(ctx, key) }() // WatchSetup value read parks on the held reply

	select {
	case <-parked:
	case <-time.After(30 * time.Second):
		t.Fatal("WatchSetup never reached the value read")
	}

	watchTx.Cancel() // cancels watchCtx; the setup read runs on watchCtx, so it must unblock NOW

	// The reply is STILL held. With the fix the read returns via watchCtx; without it, Watch hangs here.
	var got error
	select {
	case got = <-watchErr:
	case <-time.After(20 * time.Second):
		t.Fatal("Watch did not return after Cancel while the reply was held — setup read ignored watchCtx (ran on the caller ctx), leaking the slot")
	}
	if fdbCodeOf(got) != 1025 {
		t.Fatalf("a txn Cancel during the setup read must surface transaction_cancelled (1025), got %v", got)
	}

	// Now release the held reply so the connection read loop drains, then confirm the slot was freed.
	releaseIt()
	sd.setIntercept(nil)
	tx2 := db.CreateTransaction()
	tx2.SetReadVersion(rv)
	if _, _, _, _, _, err := tx2.WatchSetup(ctx, []byte(t.Name()+"_k2")); err != nil {
		t.Fatalf("the cancelled watch must release its slot; fresh WatchSetup must succeed, got %v", err)
	}
	db.db.releaseWatch() // free tx2's slot (no WatchPoll runs for it)
}
