package sqldriver_test

// Regression for a SEVERE data-loss bug: `<DML> ... OPTIONS (DRY RUN)` silently IGNORED
// the DRY RUN safety option and EXECUTED the real mutation — the exact opposite of the
// user's intent (DRY RUN means "preview, do not mutate"). `DELETE WHERE a>0 OPTIONS
// (DRY RUN)` wiped every matching row; UPDATE/INSERT likewise mutated. Java honors DRY
// RUN (AstNormalizer.visitQueryOptions → QueryPlan.setDryRun; store primitives exist in
// Go — DryRunSaveRecord/DryRunDeleteRecord — but the executor never branched on it).
//
// Until DRY RUN is wired through (TODO.md "DML DRY RUN not implemented"), it is rejected
// with 0AF00 "DRY RUN is not supported" — fail-closed, so the data is left UNCHANGED.
// Harmless options (NOCACHE / LOG QUERY) remain accepted-and-ignored.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

func TestFDB_DryRunOptionRejectedProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_dryr")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_dryr")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE dryr CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_dryr/s WITH TEMPLATE dryr")
	dsn := fmt.Sprintf("fdbsql:///testdb_dryr?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	count := func() int {
		var n int
		_ = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM t").Scan(&n)
		return n
	}
	maxA := func() int64 {
		var m sql.NullInt64
		_ = db.QueryRowContext(ctx, "SELECT MAX(a) FROM t").Scan(&m)
		return m.Int64
	}
	reset := func() {
		_, _ = db.ExecContext(ctx, "DELETE FROM t")
		mwjoMustExec(t, db, ctx, "INSERT INTO t (id, a) VALUES (1,10),(2,20),(3,30)")
	}
	rejects := func(name, stmt string, check func()) {
		t.Run(name, func(t *testing.T) {
			reset()
			_, err := db.ExecContext(ctx, stmt)
			if err == nil || !strings.Contains(err.Error(), "0AF00") {
				t.Fatalf("%s error = %v, want 0AF00 (DRY RUN not supported)", name, err)
			}
			check() // must show NO mutation happened
		})
	}
	rejects("delete_dry_run_no_data_loss", "DELETE FROM t WHERE a > 0 OPTIONS (DRY RUN)", func() {
		if c := count(); c != 3 {
			t.Errorf("after rejected DELETE DRY RUN, count = %d, want 3 (no data loss)", c)
		}
	})
	rejects("update_dry_run_no_mutation", "UPDATE t SET a = 999 OPTIONS (DRY RUN)", func() {
		if m := maxA(); m != 30 {
			t.Errorf("after rejected UPDATE DRY RUN, MAX(a) = %d, want 30 (no mutation)", m)
		}
	})
	rejects("insert_dry_run_no_mutation", "INSERT INTO t (id, a) VALUES (99, 99) OPTIONS (DRY RUN)", func() {
		if c := count(); c != 3 {
			t.Errorf("after rejected INSERT DRY RUN, count = %d, want 3 (no mutation)", c)
		}
	})
	t.Run("nocache_option_harmless_still_executes", func(t *testing.T) {
		reset()
		res, err := db.ExecContext(ctx, "DELETE FROM t WHERE id = 1 OPTIONS (NOCACHE)")
		if err != nil {
			t.Fatalf("DELETE ... OPTIONS (NOCACHE): %v", err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			t.Errorf("NOCACHE DELETE RowsAffected = %d, want 1 (harmless option, executes normally)", n)
		}
		if c := count(); c != 2 {
			t.Errorf("count = %d, want 2", c)
		}
	})
}
