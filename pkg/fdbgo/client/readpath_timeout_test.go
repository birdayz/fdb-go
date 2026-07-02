package client

import (
	"context"
	"errors"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/wire"
	"fdb.dev/pkg/fdbgo/wire/types"
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
// Determinism: the timeout is driven by a drop-reply simDialer (newDropReplyTestDB,
// simtransport_test.go) that SILENTLY DROPS every server reply once armed, combined with a small
// rpcTimeoutOverride. The reply never arrives, so the per-read timer ALWAYS
// fires — there is no timer-vs-real-reply race. (An earlier version set
// rpcTimeoutOverride = 1ns over a real connection and hoped the timer beat the
// reply; on a fast box the real getKey reply won every iteration and the
// "1007 is reachable" assertion failed in CI. Dropping the reply removes the
// race entirely.) The location cache + read version are warmed BEFORE arming,
// so the only RPC in the fault window is the read-under-test's own — locate and
// GRV (which use DefaultRPCTimeout) never run while replies are being dropped.

// assertRetryableTooOld is the precise fix guarantee under a dropped reply: the
// read CANNOT succeed (the reply is gone), so it MUST fail with a RETRYABLE
// transaction_too_old — never a raw context deadline, never the internal
// sentinel, never non-retryable all_alternatives_failed (1006).
func assertRetryableTooOld(t *testing.T, err error, what string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: unexpectedly succeeded though every server reply was dropped", what)
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
		t.Fatalf("%s: expected retryable transaction_too_old (%d), got %d", what, ErrTransactionTooOld, fdbErr.Code)
	}
}

func TestReadPath_ReplyTimeout_SurfacesRetryable(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db, dd := newDropReplyTestDB(t, ctx)

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

	// Warm the location cache for every key/range the test reads, with
	// successful reads, so once we arm the drop-reply dialer the ONLY RPCs are the
	// read-under-test's storage reads — no GetKeyServerLocations during the
	// fault window (which uses DefaultRPCTimeout and would hang on a dropped
	// reply).
	if _, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		if _, e := tx.Get(ctx, k1); e != nil {
			return nil, e
		}
		if _, _, e := tx.GetRange(ctx, k1, k3, 100); e != nil {
			return nil, e
		}
		if _, e := tx.GetKey(ctx, k1, false, 1); e != nil {
			return nil, e
		}
		return nil, nil
	}); err != nil {
		t.Fatalf("warm: %v", err)
	}

	// Pre-fetch a read version so no GRV runs during the fault window.
	rv, _, err := db.db.grvBatchers[grvBatcherDefault].getReadVersion(db.db, ctx, grvPriorityDefault, types.SpanContext{}, false, false)
	if err != nil {
		t.Fatalf("GRV: %v", err)
	}

	// Arm: every subsequent non-PING server reply is silently dropped.
	dd.armAll()

	cases := []struct {
		name string
		read func(*Transaction) error
	}{
		{"getValue", func(tx *Transaction) error { _, e := tx.Get(ctx, k1); return e }},
		{"getRange", func(tx *Transaction) error { _, _, e := tx.GetRange(ctx, k1, k3, 100); return e }},
		// Call the RAW tx.getKey, NOT public GetKey: with RYW enabled (the
		// default) GetKey resolves the selector via tx.ryw.getKeyRYW → tx.getRange
		// (transaction.go:842), which would re-exercise the range path and leave
		// the getKey three-budget timeout loop (the part of the fix unique to this
		// path) untested. The raw call hits sendGetKey directly. tx's read
		// version is already set via SetReadVersion, so skipping GetKey's
		// ensureReadVersion is fine.
		{"getKey", func(tx *Transaction) error { _, e := tx.getKey(ctx, k1, false, 1); return e }}, // firstGreaterOrEqual(k1)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tx := db.CreateTransaction()
			tx.SetReadVersion(rv)
			// Small but irrelevant to the race — the reply never comes, so the
			// timer always wins; this just keeps the bounded re-send fast.
			tx.rpcTimeoutOverride = 20 * time.Millisecond
			assertRetryableTooOld(t, tc.read(tx), tc.name)
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
