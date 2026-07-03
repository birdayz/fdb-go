package sqldriver_test

// RFC-173 W4b e2e: the correlated-scalar-subquery-in-projection seed ORDINALIZES
// when the outer is a single source, and executes correctly end-to-end. The
// ordinal-vs-name-model choice is invisible in EXPLAIN, so the seed SHAPE is
// pinned white-box (rfc173_w4b_scalar_seed_test.go); this proves the ordinal
// path returns correct rows AND that both a QUALIFIED outer ref (d.id) and a
// BARE one (id) resolve to the single outer ordinal (single-source
// alias-normalization — design condition b: the ordinal seed emits ONE field
// per column, no bare+qualified duplicates, so a qualified ref must resolve via
// the leg's RecordName span rather than a second name-model field).

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestFDB_RFC173W4b_ScalarSubqueryOrdinalSeed(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_ssos")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_ssos")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE ssos "+
		"CREATE TABLE dept (id BIGINT NOT NULL, name STRING, PRIMARY KEY (id)) "+
		"CREATE TABLE emp (id BIGINT NOT NULL, dept_id BIGINT, salary BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_ssos/s WITH TEMPLATE ssos")
	dsn := fmt.Sprintf("fdbsql:///testdb_ssos?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// dept 1: exactly one emp (salary 500) — single match, so no user LIMIT is
	// needed and the cardinality guard never fires. dept 2: no emp — the
	// LEFT-OUTER null-fill case (the inner ordinal is nullable).
	mwjoMustExec(t, db, ctx, "INSERT INTO dept (id, name) VALUES (1, 'eng'), (2, 'ops')")
	mwjoMustExec(t, db, ctx, "INSERT INTO emp (id, dept_id, salary) VALUES (10, 1, 500)")

	// QUALIFIED outer refs (d.id, d.name) over the single-source (ordinalized) outer.
	t.Run("qualified_outer_refs", func(t *testing.T) {
		var id int64
		var name string
		var sal sql.NullInt64
		if sErr := db.QueryRowContext(ctx,
			"SELECT d.id, d.name, (SELECT salary FROM emp e WHERE e.dept_id = d.id) FROM dept d WHERE d.id = 1",
		).Scan(&id, &name, &sal); sErr != nil {
			t.Fatalf("qualified outer refs: %v", sErr)
		}
		if id != 1 || name != "eng" || !sal.Valid || sal.Int64 != 500 {
			t.Fatalf("qualified = (%d, %q, valid=%v/%d), want (1, eng, valid/500)", id, name, sal.Valid, sal.Int64)
		}
	})

	// BARE outer refs (id, name) — must resolve to the SAME outer ordinals.
	t.Run("bare_outer_refs", func(t *testing.T) {
		var id int64
		var name string
		var sal sql.NullInt64
		if sErr := db.QueryRowContext(ctx,
			"SELECT id, name, (SELECT salary FROM emp e WHERE e.dept_id = d.id) FROM dept d WHERE id = 1",
		).Scan(&id, &name, &sal); sErr != nil {
			t.Fatalf("bare outer refs: %v", sErr)
		}
		if id != 1 || name != "eng" || !sal.Valid || sal.Int64 != 500 {
			t.Fatalf("bare = (%d, %q, valid=%v/%d), want (1, eng, valid/500)", id, name, sal.Valid, sal.Int64)
		}
	})

	// dept 2 has no emp → the scalar is NULL (LEFT-OUTER null-fill through the
	// nullable inner ordinal), NOT a dropped row.
	t.Run("empty_inner_null_scalar", func(t *testing.T) {
		var id int64
		var sal sql.NullInt64
		if sErr := db.QueryRowContext(ctx,
			"SELECT d.id, (SELECT salary FROM emp e WHERE e.dept_id = d.id) FROM dept d WHERE d.id = 2",
		).Scan(&id, &sal); sErr != nil {
			t.Fatalf("empty inner: %v", sErr)
		}
		if id != 2 || sal.Valid {
			t.Fatalf("empty inner = (%d, valid=%v), want (2, NULL)", id, sal.Valid)
		}
	})
}
