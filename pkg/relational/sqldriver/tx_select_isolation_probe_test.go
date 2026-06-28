package sqldriver_test

// Pins the explicit-transaction read/write isolation model and documents a known
// divergence from Java / the SQL standard:
//
//   - DML (INSERT/UPDATE/DELETE) inside BeginTx joins the explicit FDB transaction
//     (runInTx) and is atomic on Commit / undone on Rollback. (Correct.)
//   - SELECT inside BeginTx runs in a FRESH auto-commit transaction (DB.Run), NOT
//     the explicit tx — see cascades_generator.go ("DML joins ... SELECT runs in a
//     fresh auto-commit transaction"). Consequences (DIVERGENCE, see TODO.md
//     "explicit-transaction read isolation"):
//       * NO read-your-writes: a SELECT does not see the same tx's uncommitted DML.
//       * Reads add no read-conflict range, so a read-modify-write across two
//         explicit txns does NOT raise a serialization conflict (last-writer-wins).
//
// Java (setAutoCommit(false)) reads through the same FDB tx and so DOES provide
// read-your-writes. Flip these assertions if Go adopts in-tx reads.

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestFDB_TxSelectIsolationProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_txiso")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_txiso")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE txiso CREATE TABLE t (id BIGINT NOT NULL, v BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_txiso/s WITH TEMPLATE txiso")
	dsn := fmt.Sprintf("fdbsql:///testdb_txiso?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	db.SetMaxOpenConns(4)
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, v) VALUES (1, 100)")

	t.Run("no_read_your_writes_in_explicit_tx", func(t *testing.T) {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback()
		if _, err := tx.ExecContext(ctx, "UPDATE t SET v = 777 WHERE id = 1"); err != nil {
			t.Fatalf("update in tx: %v", err)
		}
		var v int64
		if err := tx.QueryRowContext(ctx, "SELECT v FROM t WHERE id = 1").Scan(&v); err != nil {
			t.Fatalf("select in tx: %v", err)
		}
		// DIVERGENCE: SELECT runs in a fresh tx → sees the pre-update value, not 777.
		if v != 100 {
			t.Errorf("in-tx SELECT after UPDATE v=%d; current driver semantics expect 100 "+
				"(no read-your-writes — SELECT auto-commits). If this is now 777, read-your-"+
				"writes was implemented: update this test and TODO.md/DIVERGENCES.md.", v)
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
