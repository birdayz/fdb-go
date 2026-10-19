package client

import (
	"context"
	"errors"
	"testing"

	"fdb.dev/pkg/fdbgo/wire"
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

// Not parallel: AllocsPerRun counts process-wide allocations. An operation
// under an uncancellable parent allocates only its readOperation: its release
// is the lease itself, not a bound-method closure.
func TestJoinedOperationLeaseAllocations(t *testing.T) {
	tx := &Transaction{}
	defer tx.Cancel()
	allocs := testing.AllocsPerRun(100, func() {
		ctx, release, owned := tx.opContextWithLease(context.Background(), false)
		if owned == nil || ctx.Done() == nil {
			t.Fatal("joined operation was not admitted")
		}
		release.release()
	})
	if allocs != 1 {
		t.Fatalf("ALLOC_JOINED_OPERATION: %v allocations per operation, want 1", allocs)
	}
}

// Every admission path's release returns the lease it admitted, exactly once.
func TestOperationReleaseEndsEveryLease(t *testing.T) {
	cancellable, cancel := context.WithCancel(context.Background())
	defer cancel()
	tx := &Transaction{}
	defer tx.Cancel()
	users := func() int {
		tx.readErrMu.Lock()
		defer tx.readErrMu.Unlock()
		return tx.readIncarnationLocked().users
	}
	outer, outerRelease, outerOwned := tx.opContextWithLease(context.Background(), false)
	borrowed, borrowedRelease, borrowedOwned := tx.opContextWithLease(outer, false)
	if borrowed != outer || borrowedOwned != nil || borrowedRelease.fn != nil || borrowedRelease.lease != nil {
		t.Fatal("a borrower received release authority")
	}
	borrowedRelease.release()
	if !outerOwned.isActive() || users() != 1 {
		t.Fatal("a borrower's release ended its owner's lease")
	}
	outerOwned.release()
	_, nestedRelease, nestedOwned := tx.opContextWithLease(outer, false)
	_, derivedRelease, derivedOwned := tx.opContextWithLease(cancellable, false)
	if nestedOwned == nil || derivedOwned == nil || users() != 2 {
		t.Fatalf("nested/derived admission: users=%d", users())
	}
	for _, r := range []opRelease{nestedRelease, derivedRelease, outerRelease} {
		r.release()
		r.release()
	}
	if nestedOwned.isActive() || derivedOwned.isActive() || users() != 0 {
		t.Fatalf("released operations kept leases: users=%d", users())
	}
}

// Not parallel: AllocsPerRun counts process-wide allocations. A new
// transaction's first span lives inline; a later span is a fresh pointee, so
// the published first span is never written again.
func TestCreateTransactionInlineSpan(t *testing.T) {
	d := &Database{db: &database{}}
	tx := d.CreateTransaction()
	defer tx.Cancel()
	if tx.spanContext.Load() != &tx.initialSpan || tx.currentSpan() != tx.initialSpan || tx.initialSpan.SpanID == 0 {
		t.Error("ALLOC_INITIAL_SPAN: the first span is not the transaction's inline span")
	}
	first := tx.initialSpan
	tx.regenerateSpan()
	if p := tx.spanContext.Load(); p == &tx.initialSpan || *p == first || tx.initialSpan != first {
		t.Fatal("regenerating the span wrote the published inline span")
	}
	allocs := testing.AllocsPerRun(100, func() { d.CreateTransaction().Cancel() })
	if allocs > 6 {
		t.Fatalf("ALLOC_INITIAL_SPAN: CreateTransaction+Cancel = %v allocations, want <= 6", allocs)
	}
}

// Not parallel: AllocsPerRun counts process-wide allocations. A batch's
// storage is reused by the next batch once flushed, holding no waiter.
func TestGRVBatchStorageReuse(t *testing.T) {
	b := &grvBatcher{}
	req := grvRequest{reply: make(chan grvResult, 1), tags: []string{"tag"}}
	cycle := func() {
		batch := b.admit(nil, req) // serial fast path: nothing in flight
		if len(batch) != 1 || batch[0].reply != req.reply {
			t.Fatalf("serial GRV was not dispatched alone: %d", len(batch))
		}
		b.finishBatch()
		b.recycleBatch(batch)
	}
	cycle()
	if len(b.spare) != 0 || cap(b.spare) == 0 {
		t.Fatalf("flushed batch storage was not kept: len=%d cap=%d", len(b.spare), cap(b.spare))
	}
	if kept := b.spare[:1][0]; kept.reply != nil || kept.tags != nil {
		t.Fatal("kept batch storage retains a completed waiter")
	}
	if allocs := testing.AllocsPerRun(100, cycle); allocs != 0 {
		t.Fatalf("ALLOC_GRV_BATCH: %v allocations per serial batch, want 0", allocs)
	}

	// The real flush recycles too, after its panic backstop has failed the batch.
	batch := b.admit(nil, req)
	b.flush(nil, batch)
	if result := <-req.reply; result.err == nil {
		t.Fatal("flush without a database did not fail its waiter")
	}
	if b.inFlight != 0 || cap(b.spare) == 0 || b.spare[:1][0].reply != nil {
		t.Fatal("flush did not recycle the cleared batch")
	}
}

// GetSliceWithError reuses PendingRange.Get's rows without a copy: each Get
// must return storage no cache or earlier result shares.
func TestPendingRangeGetReturnsOwnedRows(t *testing.T) {
	p := &PendingRange{done: closedDone, kvs: []KeyValue{{Key: []byte("a"), Value: []byte("1")}}}
	p.state.Store(pendingClaimed)
	first, _, _ := p.Get()
	second, _, _ := p.Get()
	if len(first) != 1 || len(second) != 1 || cap(first) != 1 {
		t.Fatalf("Get rows: len=%d/%d cap=%d", len(first), len(second), cap(first))
	}
	if &first[0] == &p.kvs[0] || &second[0] == &p.kvs[0] || &first[0] == &second[0] {
		t.Fatal("PendingRange.Get returned rows another holder shares")
	}
}

// A turnover's transaction_cancelled is the caller's to keep: mutating the
// error one transaction returned must not change what a later turnover reports.
func TestTurnoverCauseIsNotShared(t *testing.T) {
	for i := range 2 {
		tx := &Transaction{}
		value, pending, err := tx.GetVersionstampPending(context.Background())
		if value != nil || pending == nil || err != nil {
			t.Fatalf("versionstamp before commit = (%v, %v, %v), want a pending handle", value, pending, err)
		}
		tx.Reset()
		_, err = pending.Resolve()
		var fdbErr *wire.FDBError
		if !errors.As(err, &fdbErr) || fdbErr.Code != ErrTransactionCancelled {
			t.Fatalf("transaction %d: versionstamp after Reset = %v, want transaction_cancelled (1025)", i, err)
		}
		fdbErr.Code = 4100 // a caller may keep and change the error it received
		tx.Cancel()
	}
}
