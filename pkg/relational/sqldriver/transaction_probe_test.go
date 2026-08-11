package sqldriver_test

// Probes database/sql transaction semantics over the fdbsql driver: ROLLBACK
// undoes all changes (atomicity), COMMIT persists them, uncommitted writes are
// not visible to a separate connection (isolation), and a multi-statement tx is
// all-or-nothing.

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"
)

func TestFDB_TransactionProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	// Created DISARMED: the two single-statement subtests below cannot expire a
	// window between statements, so they are not exposed and must not be spiked —
	// they have no retry and a pre-emption would simply fail them. Only
	// multi_statement_atomic_rollback arms it, for its own transaction.
	key, clk := spikedClusterKey(t, 30*time.Second)
	clk.Disarm()
	setup := openSpiked(t, key, "/testdb_txnprobe", "")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_txnprobe")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE txnprobe "+
			"CREATE TABLE t (id BIGINT, v BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_txnprobe/s WITH TEMPLATE txnprobe")
	dsn := spikedDSN(key, "/testdb_txnprobe", "s")
	db := openSpiked(t, key, "/testdb_txnprobe", "s")
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, v) VALUES (1, 10)")

	count := func() int64 {
		var c int64
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM t").Scan(&c); err != nil {
			t.Fatalf("count: %v", err)
		}
		return c
	}

	t.Run("rollback_undoes", func(t *testing.T) {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("BeginTx: %v", err)
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO t (id, v) VALUES (100, 1000)"); err != nil {
			t.Fatalf("tx insert: %v", err)
		}
		if err := tx.Rollback(); err != nil {
			t.Fatalf("rollback: %v", err)
		}
		if got := count(); got != 1 {
			t.Errorf("after rollback count = %d, want 1 (rollback must undo)", got)
		}
	})

	t.Run("commit_persists", func(t *testing.T) {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("BeginTx: %v", err)
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO t (id, v) VALUES (101, 1010)"); err != nil {
			t.Fatalf("tx insert: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
		var v int64
		if err := db.QueryRowContext(ctx, "SELECT v FROM t WHERE id = 101").Scan(&v); err != nil {
			t.Fatalf("read committed: %v", err)
		}
		if v != 1010 {
			t.Errorf("committed value = %d, want 1010", v)
		}
	})

	t.Run("multi_statement_atomic_rollback", func(t *testing.T) {
		// FIVE statements on one read version — the most exposed transaction in
		// this file, and the only one here that carries the assumption at all.
		// The other subtests issue a single in-transaction statement, so their
		// window cannot expire between statements.
		before := count()
		clk.Rearm()
		var attemptsRun int
		retryTx(t, db, spikeOnce(clk, &attemptsRun), func(a txAttempt) error {
			for i := int64(200); i < 205; i++ {
				if _, err := a.tx.ExecContext(ctx,
					fmt.Sprintf("INSERT INTO t (id, v) VALUES (%d, %d)", i, i*10)); err != nil {
					return err
				}
			}
			return nil
		})
		mustHaveRetried(t, attemptsRun)
		// retryTx rolled the successful attempt back, which is the property under
		// test: all five inserts must vanish together.
		if got := count(); got != before {
			t.Errorf("after multi-insert rollback count = %d, want %d (all-or-nothing)", got, before)
		}
	})

	t.Run("isolation_uncommitted_not_visible", func(t *testing.T) {
		// a second connection must not see the tx's uncommitted insert.
		other, err := sql.Open("fdbsql", dsn)
		if err != nil {
			t.Fatalf("open other: %v", err)
		}
		defer other.Close()
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("BeginTx: %v", err)
		}
		defer tx.Rollback()
		if _, err := tx.ExecContext(ctx, "INSERT INTO t (id, v) VALUES (300, 3000)"); err != nil {
			t.Fatalf("tx insert: %v", err)
		}
		var c int64
		if err := other.QueryRowContext(ctx, "SELECT COUNT(*) FROM t WHERE id = 300").Scan(&c); err != nil {
			t.Fatalf("other count: %v", err)
		}
		if c != 0 {
			t.Errorf("other connection sees uncommitted row (count=%d), want 0 (isolation)", c)
		}
	})
}
