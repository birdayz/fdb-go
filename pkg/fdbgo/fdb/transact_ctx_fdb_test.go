package fdb_test

import (
	"context"
	"testing"
	"time"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
)

// TestFDB_TransactCtx_CommitNotCancelledByCtx pins RFC-090's core safety property: a
// caller-ctx cancellation must NOT cancel a dispatched commit (the commit + its
// commit_unknown_result idempotency barrier run on context.WithoutCancel). We cancel
// the ctx INSIDE the closure, so the client loop reaches Commit with the ctx already
// cancelled; the write must still commit durably. If the commit honored the ctx it
// would fail (and the write would be lost or left commit-unknown).
func TestFDB_TransactCtx_CommitNotCancelledByCtx(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	key := fdb.Key("rfc090/commit-detach/" + t.Name())
	ctx, cancel := context.WithCancel(context.Background())

	_, err := db.TransactCtx(ctx, func(tx fdb.Transaction) (any, error) {
		tx.Set(key, []byte("v"))
		cancel() // cancel mid-transaction, before the client loop commits
		return nil, nil
	})
	if err != nil {
		t.Fatalf("a dispatched commit must run detached from the caller ctx; got %v", err)
	}

	got, rerr := db.ReadTransact(func(rtx fdb.ReadTransaction) (any, error) {
		return rtx.Get(key).MustGet(), nil
	})
	if rerr != nil {
		t.Fatalf("read-back: %v", rerr)
	}
	if b, _ := got.([]byte); string(b) != "v" {
		t.Fatalf("commit-detached write not durable: got %q, want %q", b, "v")
	}
}

// TestFDB_TransactCtx_RetryLoopBoundedByCtxDeadline pins that a permanently-retryable
// error under a ctx deadline exits AT the deadline rather than looping forever (the
// default retry limit is unlimited; the caller's ctx is the bound). With the pre-RFC-090
// code the facade passed context.Background() into the loop, so this would hang.
func TestFDB_TransactCtx_RetryLoopBoundedByCtxDeadline(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	attempts := 0
	start := time.Now()
	_, err := db.TransactCtx(ctx, func(tx fdb.Transaction) (any, error) {
		attempts++
		return nil, fdb.Error{Code: 1020} // not_committed — always retryable
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a permanently-retryable error under a ctx deadline must surface an error, not loop forever")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("retry loop not bounded by ctx deadline: ran %v across %d attempts", elapsed, attempts)
	}
	if attempts < 2 {
		t.Errorf("expected the loop to retry before the deadline; got %d attempts", attempts)
	}
}
