package sqldriver_test

// Probes explicit-transaction DML atomicity via database/sql Tx: ROLLBACK discards
// all DML in the transaction; COMMIT persists it atomically (visible to a
// subsequent query). (Read-your-writes WITHIN an open tx is a separate documented
// gap — SELECT auto-commits — TODO.md; this test reads AFTER commit/rollback.)

import (
	"context"
	"testing"
	"time"
)

func TestFDB_TxCommitRollbackProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	key, clk := spikedClusterKey(t, 30*time.Second)
	setup := openSpiked(t, key, "/testdb_tcrp", "")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_tcrp")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE tcrp CREATE TABLE t (id BIGINT, a BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_tcrp/s WITH TEMPLATE tcrp")
	db := openSpiked(t, key, "/testdb_tcrp", "s")
	t.Cleanup(func() { db.Close() })
	count := func() int {
		var n int
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM t").Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		return n
	}

	// Two in-transaction statements each, so both subtests below carry the
	// unstated assumption that both finish inside the four-second MVCC budget.
	// Each re-arms the one-shot spike for its OWN transaction: the first subtest
	// consumes it, and without a re-arm the second would run clean and its retry
	// would be an arm that never executes.
	t.Run("rollback_discards", func(t *testing.T) {
		clk.Rearm()
		var attemptsRun int
		retryTx(t, db, spikeOnce(clk, &attemptsRun), func(a txAttempt) error {
			if _, err := a.tx.ExecContext(ctx, "INSERT INTO t (id, a) VALUES (1, 10)"); err != nil {
				return err
			}
			_, err := a.tx.ExecContext(ctx, "INSERT INTO t (id, a) VALUES (2, 20)")
			return err
		})
		mustHaveRetried(t, attemptsRun)
		// retryTx rolled the successful attempt back, which IS the property under
		// test here: nothing the transaction wrote may survive.
		if c := count(); c != 0 {
			t.Errorf("after ROLLBACK count = %d, want 0 (rolled-back inserts must not persist)", c)
		}
	})

	t.Run("commit_persists_atomically", func(t *testing.T) {
		clk.Rearm()
		var attemptsRun int
		// The body COMMITS as its last act, so the rollback retryTx issues
		// afterwards is a no-op on an already-finished transaction. A discarded
		// attempt never reaches the commit, so no attempt but the last one can
		// leave anything behind — which is what makes the exact count below still
		// a legitimate assertion under a retry.
		retryTx(t, db, spikeOnce(clk, &attemptsRun), func(a txAttempt) error {
			if _, err := a.tx.ExecContext(ctx, "INSERT INTO t (id, a) VALUES (3, 30)"); err != nil {
				return err
			}
			if _, err := a.tx.ExecContext(ctx, "INSERT INTO t (id, a) VALUES (4, 40)"); err != nil {
				return err
			}
			return a.tx.Commit()
		})
		mustHaveRetried(t, attemptsRun)
		if c := count(); c != 2 {
			t.Errorf("after COMMIT count = %d, want 2", c)
		}
		// committed rows are readable by value.
		var a int64
		if err := db.QueryRowContext(ctx, "SELECT a FROM t WHERE id = 3").Scan(&a); err != nil {
			t.Fatalf("read committed: %v", err)
		}
		if a != 30 {
			t.Errorf("committed id=3 a = %d, want 30", a)
		}
	})
}
