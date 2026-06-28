package sqldriver_test

// Probes correlated scalar subqueries in the SELECT projection (a Go read-side
// extension over Java) and the scalar-subquery cardinality (21000) behavior.
//
// Java enforces NO scalar-subquery cardinality (its ErrorCode enum has no 21000 /
// CARDINALITY_VIOLATION); Go ADDED 21000 enforcement (SQL-standard, stricter) but
// ONLY on the NON-correlated path. The correlated path (RFC-077 source-anchored
// join) silently takes the first inner row. This pins:
//   - correlated COUNT/MAX in SELECT return correct per-row values (the valuable,
//     correct behavior);
//   - non-correlated scalar subquery >1 row errors 21000;
//   - correlated scalar subquery >1 row currently does NOT enforce 21000 (takes a
//     row) — the documented inconsistency, see TODO.md "scalar-subquery
//     cardinality (correlated)". When that Graefe-designed decision lands, flip
//     the corr_scalar_multi_row subtest to expect 21000.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

func TestFDB_ScalarSubqueryCorrelationProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_ssqcorr")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_ssqcorr")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE ssqcorr "+
		"CREATE TABLE dept (id BIGINT NOT NULL, name STRING, PRIMARY KEY (id)) "+
		"CREATE TABLE emp (id BIGINT NOT NULL, dept_id BIGINT, salary BIGINT, PRIMARY KEY (id)) "+
		"CREATE INDEX emp_dept ON emp (dept_id)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_ssqcorr/s WITH TEMPLATE ssqcorr")
	dsn := fmt.Sprintf("fdbsql:///testdb_ssqcorr?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO dept (id, name) VALUES (1, 'eng'), (2, 'sales'), (3, 'empty')")
	// eng(1): two emps 100,200 ; sales(2): one emp 150 ; empty(3): none
	mwjoMustExec(t, db, ctx, "INSERT INTO emp (id, dept_id, salary) VALUES (1, 1, 100), (2, 1, 200), (3, 2, 150)")

	t.Run("corr_count_in_select", func(t *testing.T) {
		rows, err := db.QueryContext(ctx,
			"SELECT name, (SELECT COUNT(*) FROM emp e WHERE e.dept_id = dept.id) FROM dept ORDER BY id")
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		defer rows.Close()
		got := map[string]int64{}
		for rows.Next() {
			var name string
			var c int64
			if err := rows.Scan(&name, &c); err != nil {
				t.Fatalf("scan: %v", err)
			}
			got[name] = c
		}
		if got["eng"] != 2 || got["sales"] != 1 || got["empty"] != 0 {
			t.Errorf("corr COUNT = %v, want eng=2 sales=1 empty=0", got)
		}
	})

	t.Run("corr_max_empty_is_null", func(t *testing.T) {
		var m sql.NullInt64
		if err := db.QueryRowContext(ctx,
			"SELECT (SELECT MAX(salary) FROM emp e WHERE e.dept_id = dept.id) FROM dept WHERE id = 3").Scan(&m); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if m.Valid {
			t.Errorf("corr MAX over empty dept = %d, want NULL", m.Int64)
		}
	})

	t.Run("noncorr_multi_row_errors_21000", func(t *testing.T) {
		// Go extension (Java does not enforce): non-correlated scalar subquery
		// returning >1 row errors with cardinality violation 21000.
		_, err := db.QueryContext(ctx, "SELECT (SELECT salary FROM emp) FROM dept WHERE id = 1")
		if err == nil || !strings.Contains(err.Error(), "21000") {
			t.Errorf("non-correlated scalar >1 row error = %v, want 21000", err)
		}
	})

	t.Run("corr_scalar_multi_row_currently_unenforced", func(t *testing.T) {
		// CURRENT behavior: a correlated scalar subquery that matches >1 inner row
		// (eng has salaries 100 AND 200) does NOT raise 21000 — it returns one of
		// the rows via the RFC-077 join. This is a documented Go-extension
		// inconsistency (non-correlated enforces, correlated does not); Java
		// enforces neither. See TODO.md. Pin: returns exactly one row, value is one
		// of the dept's salaries, no error. Flip to expect 21000 when the
		// Graefe-designed decision lands.
		rows, err := db.QueryContext(ctx,
			"SELECT (SELECT salary FROM emp e WHERE e.dept_id = dept.id) FROM dept WHERE id = 1")
		if err != nil {
			t.Fatalf("correlated scalar >1 row unexpectedly errored (behavior changed — "+
				"if 21000 is now enforced, update this test): %v", err)
		}
		defer rows.Close()
		var vals []int64
		for rows.Next() {
			var v sql.NullInt64
			if err := rows.Scan(&v); err != nil {
				t.Fatalf("scan: %v", err)
			}
			if v.Valid {
				vals = append(vals, v.Int64)
			}
		}
		if len(vals) != 1 || (vals[0] != 100 && vals[0] != 200) {
			t.Errorf("correlated scalar >1 row = %v, want exactly one of [100 200] (current take-first behavior)", vals)
		}
	})
}
