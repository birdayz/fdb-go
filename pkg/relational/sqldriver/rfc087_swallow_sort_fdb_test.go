package sqldriver_test

// RFC-087 Phase E end-to-end FDB tests:
//
//   - The swallow axis (Graefe gate): a both-constant type-mismatch
//     comparison (`WHERE 5 = 'abc'`) declines to constant-fold at plan
//     time and must PLAN + RUN without crashing — the per-row error
//     channel must not turn a declined fold into a goroutine crash.
//   - The reachable computed-sort-key error case: a computed ORDER BY key
//     that overflows must propagate as SQLSTATE 22003 (executor.go's sortFn
//     threads k.ValueExpr.Evaluate's error via sortErr), NOT panic.

import (
	"context"
	"testing"

	"fdb.dev/pkg/relational/api"
)

// TestFDB_RFC087_WhereConstTypeMismatch_NoCrash pins that `WHERE 5 = 'abc'`
// — whose plan-time constant-fold declines (swallow axis) — plans and runs
// without crashing. The outcome is either zero rows or a clean *api.Error
// (both are acceptable three-valued results); the only failure modes are a
// panic/goroutine crash or a non-api error.
func TestFDB_RFC087_WhereConstTypeMismatch_NoCrash(t *testing.T) {
	t.Parallel()
	db := setupErrorTestDB(t, "/testdb_rfc087_swallow", "swallow",
		"CREATE TABLE t (id BIGINT, PRIMARY KEY (id))")
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, "INSERT INTO t (id) VALUES (1), (2)"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	rows, err := db.QueryContext(ctx, "SELECT id FROM t WHERE 5 = 'abc'")
	n := 0
	if err == nil && rows != nil {
		for rows.Next() {
			var id int64
			if err = rows.Scan(&id); err != nil {
				break
			}
			n++
		}
		if cerr := rows.Err(); err == nil {
			err = cerr
		}
		rows.Close()
	}

	// Reaching this point at all proves the query did not panic / crash.
	if err != nil {
		if asAPIError(err) == nil {
			t.Fatalf("WHERE 5='abc': non-api error %v (%T) — want a clean api.Error or 0 rows, never a crash", err, err)
		}
		t.Logf("WHERE 5='abc' returned a clean api.Error (no crash): %v", err)
		return
	}
	if n != 0 {
		t.Fatalf("WHERE 5='abc': got %d rows, want 0 (a type-mismatch comparison is never TRUE)", n)
	}
}

// TestFDB_RFC087_ComputedSortKeyOverflow_22003 pins that a computed ORDER BY
// key that overflows int64 propagates as SQLSTATE 22003, not a panic. Two
// rows are inserted so the sort comparator actually evaluates the key (a
// single-row sort never invokes the Less func).
func TestFDB_RFC087_ComputedSortKeyOverflow_22003(t *testing.T) {
	t.Parallel()
	db := setupErrorTestDB(t, "/testdb_rfc087_sortovf", "sortovf",
		"CREATE TABLE t (id BIGINT, v BIGINT, PRIMARY KEY (id))")
	ctx := context.Background()
	if _, err := db.ExecContext(ctx,
		"INSERT INTO t (id, v) VALUES (1, 9223372036854775807), (2, 9223372036854775806)"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	// v * 1000000000000 overflows int64 for both rows; the comparator
	// evaluates the computed key and surfaces ArithmeticOverflowError.
	rows, err := db.QueryContext(ctx, "SELECT id FROM t ORDER BY v * 1000000000000")
	if err == nil && rows != nil {
		for rows.Next() {
			var id int64
			if err = rows.Scan(&id); err != nil {
				break
			}
		}
		if cerr := rows.Err(); err == nil {
			err = cerr
		}
		rows.Close()
	}

	if err == nil {
		t.Fatal("ORDER BY v * 1000000000000: expected 22003 overflow error, got nil")
	}
	got := asAPIError(err)
	if got == nil {
		t.Fatalf("ORDER BY overflow: non-api error %v (%T) — want a clean 22003", err, err)
	}
	if got.Code != api.ErrCodeNumericValueOutOfRange {
		t.Fatalf("ORDER BY overflow: code = %q, want 22003 NUMERIC_VALUE_OUT_OF_RANGE (full: %v)", got.Code, err)
	}
}
