package sqldriver_test

// KNOWN-ISSUE sentinel — UPDATE of a PRIMARY KEY column (TODO.md "UPDATE of PK
// column surfaces a leaky XXXXX error").
//
// `UPDATE t SET id = <new> WHERE id = <old>` does not relocate the record; the
// executor applies the SET to the proto (including the PK field) then calls
// SaveRecordWithOptions(..., ErrorIfNotExists), which targets the NEW pk and fails
// the existence check → a LEAKY internal error (SQLSTATE XXXXX / ErrCodeUnknown,
// "record does not exist"; executor.go:2474, whose comment even assumes "an UPDATE
// does not change the PK"). It is fail-CLOSED — the table is left UNCHANGED, no
// corruption. The right end-state is either a clean user-facing rejection (proper
// SQLSTATE, "cannot update primary key") or record relocation (delete+insert),
// matching Java — pending a Java-behavior check + executor review. This test pins
// the two invariants that matter now: (1) the operation is rejected (not silently
// applied), and (2) NO data is corrupted.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"testing"
)

func TestFDB_UpdatePrimaryKeyProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_upkp")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_upkp")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE upkp CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_upkp/s WITH TEMPLATE upkp")
	dsn := fmt.Sprintf("fdbsql:///testdb_upkp?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, a) VALUES (1,10),(2,20)")

	snapshot := func() []string {
		rows, err := db.QueryContext(ctx, "SELECT id, a FROM t")
		if err != nil {
			t.Fatalf("snapshot: %v", err)
		}
		defer rows.Close()
		var o []string
		for rows.Next() {
			var id, a int64
			_ = rows.Scan(&id, &a)
			o = append(o, fmt.Sprintf("%d:%d", id, a))
		}
		sort.Strings(o)
		return o
	}
	before := snapshot()

	t.Run("pk_update_rejected", func(t *testing.T) {
		// Currently rejected with the leaky XXXXX error. Pin that it does NOT
		// succeed; do not over-pin the exact (leaky) wording beyond "record does not
		// exist", so a future clean-error fix only needs to update this assertion.
		_, err := db.ExecContext(ctx, "UPDATE t SET id = 99 WHERE id = 1")
		if err == nil {
			t.Errorf("UPDATE SET id (PK) unexpectedly succeeded; want a rejection " +
				"(today leaky XXXXX 'record does not exist'; ideally a clean SQLSTATE or relocation)")
		}
	})
	t.Run("no_data_corruption", func(t *testing.T) {
		after := snapshot()
		if len(after) != len(before) {
			t.Fatalf("row count changed: before %v, after %v", before, after)
		}
		for i := range before {
			if before[i] != after[i] {
				t.Errorf("table mutated by failed PK update: before %v, after %v", before, after)
			}
		}
	})
	t.Run("non_pk_update_still_works", func(t *testing.T) {
		// sanity: a normal (non-PK) UPDATE is unaffected.
		if _, err := db.ExecContext(ctx, "UPDATE t SET a = 11 WHERE id = 1"); err != nil {
			t.Fatalf("non-PK update: %v", err)
		}
		var a int64
		if err := db.QueryRowContext(ctx, "SELECT a FROM t WHERE id = 1").Scan(&a); err != nil {
			t.Fatalf("read: %v", err)
		}
		if a != 11 {
			t.Errorf("non-PK update id=1 a = %d, want 11", a)
		}
	})
}
