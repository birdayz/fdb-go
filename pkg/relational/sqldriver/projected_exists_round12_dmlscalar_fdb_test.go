package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// TestFDB_ProjectedExistsRound12_DMLScalar pins that the WHERE-scalar-EXISTS
// backstop also runs on the DML path. A DELETE/UPDATE whose WHERE buries an
// EXISTS in a scalar expression (CASE/comparison) was silently mis-evaluated
// (constant false → wrong rows affected) because the DML WHERE-build path
// differs from the SELECT PlanVisitor path.
func TestFDB_ProjectedExistsRound12_DMLScalar(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_pexr12dmlsc")
	mustExec(t, setup, ctx, "CREATE DATABASE /testdb_pexr12dmlsc")
	mustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE pexr12dmlsc_tmpl "+
		"CREATE TABLE t1 (id BIGINT NOT NULL, col1 BIGINT, PRIMARY KEY (id)) "+
		"CREATE TABLE t2 (id BIGINT NOT NULL, fk BIGINT, PRIMARY KEY (id)) "+
		"CREATE TABLE t3 (id BIGINT NOT NULL, v BIGINT, PRIMARY KEY (id))")
	mustExec(t, setup, ctx, "CREATE SCHEMA /testdb_pexr12dmlsc/s WITH TEMPLATE pexr12dmlsc_tmpl")
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///testdb_pexr12dmlsc?cluster_file=%s&schema=s", clusterFilePath))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mustExec(t, db, ctx, "INSERT INTO t2 VALUES (100, 2)")

	const want = "EXISTS nested in a scalar expression is not yet supported"
	scalarExists := "CASE WHEN EXISTS (SELECT 1 FROM t2 WHERE t2.fk = t1.id) THEN 1 ELSE 0 END = 1"

	remaining := func() int {
		var n int
		db.QueryRowContext(ctx, "SELECT COUNT(*) FROM t1").Scan(&n)
		return n
	}

	t.Run("delete_where_scalar_exists_rejected", func(t *testing.T) {
		mustExec(t, db, ctx, "DELETE FROM t1")
		mustExec(t, db, ctx, "INSERT INTO t1 VALUES (1, 10), (2, 20), (3, 30)")
		_, err := db.ExecContext(ctx, "DELETE FROM t1 WHERE "+scalarExists)
		if err == nil {
			t.Fatalf("DELETE WHERE scalar-EXISTS succeeded (remaining=%d) — must reject cleanly", remaining())
		}
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("expected %q, got: %v", want, err)
		}
		if remaining() != 3 {
			t.Fatalf("rows changed despite rejection: remaining=%d", remaining())
		}
	})

	t.Run("update_where_scalar_exists_rejected", func(t *testing.T) {
		mustExec(t, db, ctx, "DELETE FROM t1")
		mustExec(t, db, ctx, "INSERT INTO t1 VALUES (1, 10), (2, 20), (3, 30)")
		_, err := db.ExecContext(ctx, "UPDATE t1 SET col1 = 99 WHERE "+scalarExists)
		if err == nil {
			t.Fatalf("UPDATE WHERE scalar-EXISTS succeeded — must reject cleanly")
		}
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("expected %q, got: %v", want, err)
		}
	})

	// INSERT … SELECT whose SELECT-body WHERE buries an EXISTS in a scalar → reject
	// (the body is rebuilt through a path that bypasses the per-statement guard).
	t.Run("insert_select_where_scalar_exists_rejected", func(t *testing.T) {
		mustExec(t, db, ctx, "DELETE FROM t1")
		mustExec(t, db, ctx, "INSERT INTO t1 VALUES (1, 10), (2, 20), (3, 30)")
		_, err := db.ExecContext(ctx,
			"INSERT INTO t3 SELECT id, col1 FROM t1 WHERE "+scalarExists)
		if err == nil {
			t.Fatalf("INSERT…SELECT WHERE scalar-EXISTS succeeded — must reject cleanly")
		}
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("expected %q, got: %v", want, err)
		}
	})

	// Control: direct DELETE WHERE EXISTS still works (deletes only id 2).
	t.Run("control_delete_where_exists", func(t *testing.T) {
		mustExec(t, db, ctx, "DELETE FROM t1")
		mustExec(t, db, ctx, "INSERT INTO t1 VALUES (1, 10), (2, 20), (3, 30)")
		mustExec(t, db, ctx, "DELETE FROM t1 WHERE EXISTS (SELECT 1 FROM t2 WHERE t2.fk = t1.id)")
		if remaining() != 2 {
			t.Fatalf("direct DELETE WHERE EXISTS: remaining=%d want 2", remaining())
		}
	})

	// Control: direct INSERT … SELECT WHERE EXISTS still works (inserts id 2).
	t.Run("control_insert_select_where_exists", func(t *testing.T) {
		mustExec(t, db, ctx, "DELETE FROM t1")
		mustExec(t, db, ctx, "DELETE FROM t3")
		mustExec(t, db, ctx, "INSERT INTO t1 VALUES (1, 10), (2, 20), (3, 30)")
		mustExec(t, db, ctx,
			"INSERT INTO t3 SELECT id, col1 FROM t1 WHERE EXISTS (SELECT 1 FROM t2 WHERE t2.fk = t1.id)")
		var n int
		db.QueryRowContext(ctx, "SELECT COUNT(*) FROM t3").Scan(&n)
		if n != 1 {
			t.Fatalf("direct INSERT…SELECT WHERE EXISTS: inserted %d rows want 1", n)
		}
	})
}
