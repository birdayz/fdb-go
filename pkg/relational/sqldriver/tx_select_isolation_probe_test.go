package sqldriver_test

// Pins the explicit-transaction read/write isolation model (RFC-198):
//
//   - DML (INSERT/UPDATE/DELETE) inside BeginTx joins the explicit FDB transaction
//     and is atomic on Commit / undone on Rollback.
//   - SELECT inside BeginTx reads through the SAME transaction (read-your-writes,
//     and the reads add read-conflict ranges) — Java's shape: setAutoCommit(false)
//     reads through conn.getTransaction() (BackingRecordStore.java:235).
//
// The serialization half (a read-modify-write across two explicit transactions
// conflicts with 40001 instead of losing a write) is pinned separately in
// tx_isolation_rfc198_fdb_test.go.

import (
	"context"
	"testing"
	"time"
)

func TestFDB_TxSelectIsolationProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	// A one-shot clock spike, so the read-your-writes subtest below actually
	// exercises its retry on every run rather than only under load. Arming it for
	// the whole test is safe: preflightTxBudget runs under `if r.tx != nil`, so
	// the DDL and the seed INSERT — all autocommit — never meet it, and the two
	// single-statement subtests below never open a second read page.
	key, clk := spikedClusterKey(t, 30*time.Second)
	setup := openSpiked(t, key, "/testdb_txiso", "")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_txiso")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE txiso CREATE TABLE t (id BIGINT, v BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_txiso/s WITH TEMPLATE txiso")
	db := openSpiked(t, key, "/testdb_txiso", "s")
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, v) VALUES (1, 100)")

	t.Run("read_your_writes_in_explicit_tx", func(t *testing.T) {
		// TWO statements on one read version, so the transaction carries the
		// unstated assumption that both finish inside the four-second budget. The
		// UPDATE's own record load opens the MVCC window; the SELECT is then a
		// read page, and a read page is where the driver pre-empts. Retrying the
		// WHOLE transaction is what keeps this correct: the UPDATE is re-applied
		// inside the new transaction, so the value the SELECT reads back is one
		// this transaction actually wrote rather than a leftover from a dead one.
		var v int64
		var attemptsRun int
		retryTx(t, db, spikeOnce(clk, &attemptsRun), func(a txAttempt) error {
			v = 0
			if _, err := a.tx.ExecContext(ctx, "UPDATE t SET v = 777 WHERE id = 1"); err != nil {
				return err
			}
			return a.tx.QueryRowContext(ctx, "SELECT v FROM t WHERE id = 1").Scan(&v)
		})
		mustHaveRetried(t, attemptsRun)
		if v != 777 {
			t.Errorf("in-tx SELECT after UPDATE v=%d, want 777: a SELECT inside an explicit "+
				"transaction must read through that transaction (read-your-writes, RFC-198 "+
				"Decision 1). 100 means it ran in a fresh auto-commit transaction again.", v)
		}
	})

	t.Run("dml_still_atomic_on_commit", func(t *testing.T) {
		// the WRITE side IS transactional: a committed in-tx UPDATE persists.
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if _, err := tx.ExecContext(ctx, "UPDATE t SET v = 500 WHERE id = 1"); err != nil {
			t.Fatalf("update: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
		var v int64
		if err := db.QueryRowContext(ctx, "SELECT v FROM t WHERE id = 1").Scan(&v); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if v != 500 {
			t.Errorf("committed in-tx UPDATE = %d, want 500 (writes ARE transactional)", v)
		}
	})

	t.Run("dml_undone_on_rollback", func(t *testing.T) {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if _, err := tx.ExecContext(ctx, "UPDATE t SET v = 12345 WHERE id = 1"); err != nil {
			t.Fatalf("update: %v", err)
		}
		if err := tx.Rollback(); err != nil {
			t.Fatalf("rollback: %v", err)
		}
		var v int64
		if err := db.QueryRowContext(ctx, "SELECT v FROM t WHERE id = 1").Scan(&v); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if v != 500 {
			t.Errorf("after rollback v = %d, want 500 (rollback undoes the in-tx write)", v)
		}
	})
}
