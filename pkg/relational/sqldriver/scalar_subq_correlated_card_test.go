package sqldriver_test

// Pins the SQL cardinality contract for a CORRELATED scalar subquery in a
// projection: `SELECT (SELECT salary FROM emp e WHERE e.dept_id = d.id) FROM dept d`.
//
// The uncorrelated path already errors 21000 on >1 row (see
// scalar_subq_overflow_probe_test.go). The correlated path used to inject a
// default LIMIT 1 that SILENTLY TRUNCATED a multi-match to an arbitrary first row
// (non-deterministic without ORDER BY). The fix leaves the inner uncapped and
// enforces at-most-one via a strict FirstOrDefault barrier in the join lowering:
//   - >1 matching inner rows for some outer row  -> 21000 cardinality violation
//   - exactly one matching row                   -> that row's value
//   - zero matching rows                         -> NULL (not an error)
//   - an explicit user LIMIT                      -> respected (truncation is the
//     user's deliberate intent; no 21000)
//
// The strict barrier runs fresh PER OUTER ROW (the FlatMap re-executes the inner
// per outer row), so the at-most-one check is enforced per outer row.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

func TestFDB_ScalarSubqCorrelatedCardinality(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_sscc")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_sscc")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE sscc "+
		"CREATE TABLE dept (id BIGINT NOT NULL, PRIMARY KEY (id)) "+
		"CREATE TABLE emp (id BIGINT NOT NULL, dept_id BIGINT, salary BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_sscc/s WITH TEMPLATE sscc")
	dsn := fmt.Sprintf("fdbsql:///testdb_sscc?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// dept 1: two emps (multi-match) — the cardinality-violation case.
	// dept 2: exactly one emp — the single-value case.
	// dept 3: no emps — the empty→NULL case.
	mwjoMustExec(t, db, ctx, "INSERT INTO dept (id) VALUES (1), (2), (3)")
	mwjoMustExec(t, db, ctx, "INSERT INTO emp (id, dept_id, salary) VALUES "+
		"(10, 1, 100), (11, 1, 200), (20, 2, 500)")

	// correlated multi-row → 21000. A dept with two matching emps must ERROR,
	// not silently return one arbitrary salary.
	t.Run("multi_row_21000", func(t *testing.T) {
		q := "SELECT (SELECT salary FROM emp e WHERE e.dept_id = d.id) FROM dept d"
		rows, qErr := db.QueryContext(ctx, q)
		if qErr == nil {
			// Some engines only surface the violation while streaming rows.
			var got int
			for rows.Next() {
				var v sql.NullInt64
				if sErr := rows.Scan(&v); sErr != nil {
					qErr = sErr
					break
				}
				got++
			}
			if qErr == nil {
				qErr = rows.Err()
			}
			rows.Close()
			if qErr == nil {
				t.Fatalf("multi-row correlated scalar subquery returned %d rows, want error 21000", got)
			}
		}
		if !strings.Contains(qErr.Error(), "21000") {
			t.Fatalf("error = %v, want 21000 cardinality violation", qErr)
		}
	})

	// correlated single-row → the value. dept 2 has exactly one emp.
	t.Run("single_row_value", func(t *testing.T) {
		var v sql.NullInt64
		if sErr := db.QueryRowContext(ctx,
			"SELECT (SELECT salary FROM emp e WHERE e.dept_id = d.id) FROM dept d WHERE d.id = 2",
		).Scan(&v); sErr != nil {
			t.Fatalf("scan: %v", sErr)
		}
		if !v.Valid || v.Int64 != 500 {
			t.Fatalf("single-row correlated scalar = %v (valid=%v), want 500", v.Int64, v.Valid)
		}
	})

	// correlated empty → NULL. dept 3 has no emps; the scalar must be NULL, not
	// an error and not a dropped row.
	t.Run("empty_is_null", func(t *testing.T) {
		var v sql.NullInt64
		if sErr := db.QueryRowContext(ctx,
			"SELECT (SELECT salary FROM emp e WHERE e.dept_id = d.id) FROM dept d WHERE d.id = 3",
		).Scan(&v); sErr != nil {
			t.Fatalf("scan: %v", sErr)
		}
		if v.Valid {
			t.Fatalf("empty correlated scalar = %v, want NULL", v.Int64)
		}
	})

	// explicit user LIMIT unchanged: a user-written LIMIT 1 is the user's
	// deliberate truncation intent — the top-1 salary is returned with NO 21000,
	// even though dept 1 has multiple matching emps.
	t.Run("user_limit_respected", func(t *testing.T) {
		var v sql.NullInt64
		if sErr := db.QueryRowContext(ctx,
			"SELECT (SELECT salary FROM emp e WHERE e.dept_id = d.id ORDER BY salary DESC LIMIT 1) FROM dept d WHERE d.id = 1",
		).Scan(&v); sErr != nil {
			t.Fatalf("scan: %v", sErr)
		}
		if !v.Valid || v.Int64 != 200 {
			t.Fatalf("user LIMIT 1 correlated scalar = %v (valid=%v), want 200 (top-1, no 21000)", v.Int64, v.Valid)
		}
	})
}

// TestFDB_ScalarSubqCorrelatedCardinality_SurvivesPushdown is a defensive
// design-review pin (RFC-173 W4b): the strict-single quantifier flag must SURVIVE inner-predicate planning
// (pushdown / SARG). With an index on emp(dept_id) the correlation `e.dept_id =
// d.id` becomes an index SARG pushed into the scan; if any planning step minted a
// fresh SelectExpression that dropped `strictSingle`, the strict FirstOrDefault
// would be lost and a >1-row match would silently truncate instead of erroring.
// The multi-match case must STILL raise 21000 with the index present.
func TestFDB_ScalarSubqCorrelatedCardinality_SurvivesPushdown(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_sscp")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_sscp")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE sscp "+
		"CREATE TABLE dept (id BIGINT NOT NULL, PRIMARY KEY (id)) "+
		"CREATE TABLE emp (id BIGINT NOT NULL, dept_id BIGINT, salary BIGINT, PRIMARY KEY (id)) "+
		"CREATE INDEX emp_dept ON emp (dept_id)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_sscp/s WITH TEMPLATE sscp")
	dsn := fmt.Sprintf("fdbsql:///testdb_sscp?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	mwjoMustExec(t, db, ctx, "INSERT INTO dept (id) VALUES (1)")
	mwjoMustExec(t, db, ctx, "INSERT INTO emp (id, dept_id, salary) VALUES (10, 1, 100), (11, 1, 200)")

	// The correlation SARGs into the emp_dept index; the strict flag must survive
	// that pushdown so the two-match case still errors 21000.
	var v sql.NullInt64
	err = db.QueryRowContext(ctx,
		"SELECT (SELECT salary FROM emp e WHERE e.dept_id = d.id) FROM dept d WHERE d.id = 1",
	).Scan(&v)
	if err == nil || !strings.Contains(err.Error(), "21000") {
		t.Fatalf("multi-match correlated scalar with index-SARG pushdown error = %v, want 21000 "+
			"(strictSingle flag must survive inner-predicate pushdown)", err)
	}
}
