package sqldriver_test

// Probes UPDATE assignment semantics: a self-referencing update (SET v = v+1)
// reads the old value, and a multi-column update (SET v = v*2, w = w+v) computes
// BOTH right-hand sides from the OLD row (SQL simultaneous assignment) — w must
// use the pre-update v, not the just-assigned one. Also UPDATE-all (no WHERE).

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestFDB_UpdateAssignmentProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_updassign")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_updassign")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE updassign CREATE TABLE t (id BIGINT NOT NULL, v BIGINT, w BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_updassign/s WITH TEMPLATE updassign")
	dsn := fmt.Sprintf("fdbsql:///testdb_updassign?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	vw := func(id int) (int64, int64) {
		var v, w int64
		if err := db.QueryRowContext(ctx, fmt.Sprintf("SELECT v, w FROM t WHERE id = %d", id)).Scan(&v, &w); err != nil {
			t.Fatalf("scan: %v", err)
		}
		return v, w
	}

	t.Run("self_reference_increment", func(t *testing.T) {
		mwjoMustExec(t, db, ctx, "INSERT INTO t (id, v, w) VALUES (1, 10, 100)")
		mwjoMustExec(t, db, ctx, "UPDATE t SET v = v + 1 WHERE id = 1")
		if v, _ := vw(1); v != 11 {
			t.Errorf("after SET v=v+1, v = %d, want 11", v)
		}
	})

	t.Run("multi_column_simultaneous", func(t *testing.T) {
		mwjoMustExec(t, db, ctx, "INSERT INTO t (id, v, w) VALUES (2, 10, 100)")
		// SET v = v*2, w = w + v : v→20, w→ 100 + OLD v(10) = 110 (NOT 120).
		mwjoMustExec(t, db, ctx, "UPDATE t SET v = v * 2, w = w + v WHERE id = 2")
		v, w := vw(2)
		if v != 20 {
			t.Errorf("v after multi-update = %d, want 20", v)
		}
		if w != 110 {
			t.Errorf("w after multi-update = %d, want 110 (OLD v=10; simultaneous assignment, not 120)", w)
		}
	})

	t.Run("update_all_no_where", func(t *testing.T) {
		mwjoMustExec(t, db, ctx, "INSERT INTO t (id, v, w) VALUES (3, 1, 1), (4, 2, 2)")
		mwjoMustExec(t, db, ctx, "UPDATE t SET w = 999")
		// genuine unconditional UPDATE — every row gets w=999.
		for _, id := range []int{1, 2, 3, 4} {
			if _, w := vw(id); w != 999 {
				t.Errorf("id=%d w = %d after unconditional UPDATE, want 999", id, w)
			}
		}
	})
}
