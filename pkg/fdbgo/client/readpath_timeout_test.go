package client

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"
)

// The read path imposes a per-RPC reply timeout (DefaultRPCTimeout) that
// libfdb_c does NOT have: loadBalance re-sends a slow-but-alive storage
// server until it replies or the transaction's read version ages out and the
// server itself returns transaction_too_old. The bug these tests pin: on a
// reply timeout the client must NOT leak a terminal error to the application
// — not a raw context.DeadlineExceeded (the range path did exactly this; it
// killed the 10M SPFresh soak at ~4.9M with "context deadline exceeded"), not
// the internal errReplyTimeout sentinel, and not a non-retryable
// all_alternatives_failed (1006). A read that cannot complete must surface a
// RETRYABLE transaction_too_old (1007) so the Transact loop retries with a
// fresh read version — the observable libfdb_c contract.
//
// Driven by tx.rpcTimeoutOverride = 1ns: the reply cannot arrive that fast, so
// reads time out and re-send (bounded) to 1007. The GRV uses DefaultRPCTimeout
// (unchanged), so ensureReadVersion still succeeds — only the reads time out.
// (At 1ns the select between "timer already expired" and "reply in flight" can,
// under heavy parallel scheduling, occasionally observe the reply first and
// succeed — so the load-bearing invariant asserted on every read is the
// ABSENCE of a terminal leak; that 1007 is actually reached is pinned
// separately by a short loop, where it dominates.)

// assertNoTerminalLeak is the precise fix guarantee: a read either succeeds
// (nil) or fails with a RETRYABLE transaction_too_old — never a raw context
// deadline, never the internal sentinel, never non-retryable 1006.
func assertNoTerminalLeak(t *testing.T, err error, what string) {
	t.Helper()
	if err == nil {
		return // the reply won the 1ns race; still a valid (non-leaking) outcome
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("%s: leaked raw context.DeadlineExceeded (terminal) — must be a retryable FDB error: %v", what, err)
	}
	if isReplyTimeout(err) {
		t.Fatalf("%s: leaked the internal errReplyTimeout sentinel — it must never escape the client: %v", what, err)
	}
	var fdbErr *wire.FDBError
	if !errors.As(err, &fdbErr) {
		t.Fatalf("%s: expected a *wire.FDBError, got %T: %v", what, err, err)
	}
	if fdbErr.Code == ErrAllAlternativesFailed {
		t.Fatalf("%s: surfaced non-retryable all_alternatives_failed (1006); the read path must absorb it and surface a retryable error", what)
	}
	if fdbErr.Code != ErrTransactionTooOld {
		t.Fatalf("%s: expected transaction_too_old (%d) or success, got %d", what, ErrTransactionTooOld, fdbErr.Code)
	}
}

func TestReadPath_ReplyTimeout_SurfacesRetryable(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	// Seed keys committed by a prior transaction (so a fresh read txn reads
	// them from the server, not from its own RYW write set).
	k1 := []byte(t.Name() + "/k1")
	k2 := []byte(t.Name() + "/k2")
	k3 := []byte(t.Name() + "/k3")
	if _, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(k1, []byte("v1"))
		tx.Set(k2, []byte("v2"))
		return nil, nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	newTimedOutTx := func() *Transaction {
		tx := db.CreateTransaction()
		if err := tx.ensureReadVersion(ctx); err != nil { // GRV uses DefaultRPCTimeout, succeeds
			t.Fatalf("ensureReadVersion: %v", err)
		}
		tx.rpcTimeoutOverride = time.Nanosecond // every read reply is "too late"
		return tx
	}

	// Each read path: never a terminal leak, AND across a short loop the
	// retryable transaction_too_old is actually reached at least once (the
	// timeout dominates the 1ns race).
	cases := []struct {
		name string
		read func(*Transaction) error
	}{
		{"getValue", func(tx *Transaction) error { _, e := tx.Get(ctx, k1); return e }},
		{"getRange", func(tx *Transaction) error { _, _, e := tx.GetRange(ctx, k1, k3, 100); return e }},
		{"getKey", func(tx *Transaction) error { _, e := tx.getKey(ctx, k1, false, 1); return e }}, // firstGreaterOrEqual(k1)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sawTooOld := false
			for i := 0; i < 8; i++ {
				err := tc.read(newTimedOutTx())
				assertNoTerminalLeak(t, err, tc.name)
				var fdbErr *wire.FDBError
				if errors.As(err, &fdbErr) && fdbErr.Code == ErrTransactionTooOld {
					sawTooOld = true
				}
			}
			if !sawTooOld {
				t.Fatalf("%s: never surfaced transaction_too_old across 8 timed-out reads — the retryable timeout path is unreachable", tc.name)
			}
		})
	}
}

// With the normal timeout the same reads succeed — proving the 1ns override is
// what drives the timeout, not a broken read path.
func TestReadPath_NormalTimeout_Succeeds(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	k := []byte(t.Name() + "/k")
	if _, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(k, []byte("v"))
		return nil, nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	got, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, k) // default timeout — must succeed
	})
	if err != nil {
		t.Fatalf("read with default timeout: %v", err)
	}
	if string(got.([]byte)) != "v" {
		t.Fatalf("got %q, want %q", got, "v")
	}
}
