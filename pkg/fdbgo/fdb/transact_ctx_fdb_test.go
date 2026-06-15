package fdb_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
)

// TestFDB_TransactCtx_CancelDuringFnAbortsBeforeCommit pins the cancellation contract
// (RFC-090 + codex review of PR #272): a cancel/deadline that arrives DURING fn — i.e.
// BEFORE the commit is dispatched — must abort the transaction WITHOUT committing.
//
// At that point no commit RPC has been sent, so there is no commit_unknown_result
// hazard to protect against: aborting is clean and the write simply does not apply.
// ctx bounds the transaction's reads/GRV and the commit, so the client loop must not
// run the commit path (incl. pre-commit ensureReadVersion) on an already-expired ctx.
// The complementary property — that an *in-flight* commit (already dispatched) is NOT
// torn by a late cancel (commit + commit_unknown_result barrier run on
// context.WithoutCancel) — is preserved in the code after this abort point.
func TestFDB_TransactCtx_CancelDuringFnAbortsBeforeCommit(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	key := fdb.Key("rfc090/cancel-before-commit/" + t.Name())
	ctx, cancel := context.WithCancel(context.Background())

	_, err := db.TransactCtx(ctx, func(tx fdb.WritableTransaction) (any, error) {
		tx.Set(key, []byte("v"))
		cancel() // cancel during fn, before the client loop dispatches the commit
		return nil, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancel before commit-dispatch must abort with context.Canceled; got %v", err)
	}

	// The write must NOT have committed — no commit RPC was dispatched.
	got, rerr := db.ReadTransact(func(rtx fdb.ReadTransaction) (any, error) {
		return rtx.Get(key).MustGet(), nil
	})
	if rerr != nil {
		t.Fatalf("read-back: %v", rerr)
	}
	if b, _ := got.([]byte); b != nil {
		t.Fatalf("a cancelled-before-commit write must not be durable; got %q", b)
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
	_, err := db.TransactCtx(ctx, func(tx fdb.WritableTransaction) (any, error) {
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
