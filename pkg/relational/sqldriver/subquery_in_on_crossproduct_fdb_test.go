package sqldriver_test

// Regression for the pre-existing materialized-NLJ bug: a compound JOIN ON clause
// whose conjunct is a subquery (IN-subquery or scalar-subquery) was silently
// DROPPED at translation — the ON resolver installs no SubqueryPlanner, so
// WalkPredicate declined the shape, a permissive `continue` dropped the entire ON
// predicate, and the join degraded to a CROSS PRODUCT (silent wrong rows,
// TODO.md "Known gaps").
//
// Go (like Java) does not support IN-subqueries or correlated scalar subqueries
// anywhere. The fix is fail-CLOSED: reject these ON shapes cleanly with
// ErrCodeUnsupportedQuery instead of dropping them. EXISTS-in-ON IS supported
// (Java parity) — pinned separately in exists_in_on_fdb_test.go.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"testing"

	"fdb.dev/pkg/relational/api"
)

func siCanon(a, c sql.NullInt64) string {
	render := func(v sql.NullInt64) string {
		if !v.Valid {
			return "NULL"
		}
		return fmt.Sprintf("%d", v.Int64)
	}
	return render(a) + "|" + render(c)
}

func siScanRows(t *testing.T, rows *sql.Rows) []string {
	t.Helper()
	var got []string
	for rows.Next() {
		var a, c sql.NullInt64
		if err := rows.Scan(&a, &c); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, siCanon(a, c))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	sort.Strings(got)
	return got
}

// assertUnsupported runs q and asserts it fails cleanly with
// ErrCodeUnsupportedQuery (0AF00) — NOT a silently-wrong cross product.
func assertUnsupported(t *testing.T, db *sql.DB, ctx context.Context, q string) {
	t.Helper()
	rows, err := db.QueryContext(ctx, q)
	if err == nil {
		// Some drivers defer the error to the first Next()/Scan.
		defer rows.Close()
		if rows.Next() {
			var a, c sql.NullInt64
			_ = rows.Scan(&a, &c)
			t.Fatalf("expected clean rejection, but query returned rows (silent cross-product?): first=%s", siCanon(a, c))
		}
		err = rows.Err()
		if err == nil {
			t.Fatalf("expected clean rejection (0AF00), got no error and no rows")
		}
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("error is not *api.Error: %T %v", err, err)
	}
	if apiErr.Code != api.ErrCodeUnsupportedQuery {
		t.Fatalf("error code = %s, want %s (0AF00 UNSUPPORTED_QUERY)", apiErr.Code, api.ErrCodeUnsupportedQuery)
	}
}

func TestFDB_SubqueryInOn_RejectedCleanly(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_subq_on")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_subq_on")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE subq_on "+
			"CREATE TABLE a (id BIGINT NOT NULL, PRIMARY KEY (id)) "+
			"CREATE TABLE b (id BIGINT NOT NULL, a_id BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE c (id BIGINT NOT NULL, a_id BIGINT, w BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE d (id BIGINT NOT NULL, b_id BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX b_a_id ON b (a_id) "+
			"CREATE INDEX c_a_id ON c (a_id)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_subq_on/s WITH TEMPLATE subq_on")
	dsn := fmt.Sprintf("fdbsql:///testdb_subq_on?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	mwjoMustExec(t, db, ctx, "INSERT INTO a (id) VALUES (1), (2)")
	mwjoMustExec(t, db, ctx, "INSERT INTO b (id, a_id) VALUES (10, 1), (20, 2)")
	mwjoMustExec(t, db, ctx, "INSERT INTO c (id, a_id, w) VALUES (50, 1, 999), (51, 2, 888)")
	mwjoMustExec(t, db, ctx, "INSERT INTO d (id, b_id) VALUES (1, 999), (2, 888)")

	// --- The bug: subquery in ON must be rejected cleanly, never a cross product.

	t.Run("left_in_subquery_on", func(t *testing.T) {
		assertUnsupported(t, db, ctx, "SELECT a.id, c.id FROM a JOIN b ON b.a_id = a.id "+
			"LEFT JOIN c ON c.a_id = a.id AND c.w IN (SELECT d.b_id FROM d WHERE d.id = a.id + 999)")
	})
	t.Run("left_scalar_subquery_on", func(t *testing.T) {
		assertUnsupported(t, db, ctx, "SELECT a.id, c.id FROM a JOIN b ON b.a_id = a.id "+
			"LEFT JOIN c ON c.a_id = a.id AND c.w > (SELECT MAX(d.b_id) FROM d WHERE d.id = a.id + 999)")
	})
	t.Run("inner_in_subquery_on", func(t *testing.T) {
		assertUnsupported(t, db, ctx, "SELECT a.id, c.id FROM a JOIN b ON b.a_id = a.id "+
			"JOIN c ON c.a_id = a.id AND c.w IN (SELECT d.b_id FROM d WHERE d.id = a.id + 999)")
	})
	t.Run("sole_in_subquery_on", func(t *testing.T) {
		assertUnsupported(t, db, ctx, "SELECT a.id, c.id FROM a JOIN b ON b.a_id = a.id "+
			"LEFT JOIN c ON c.w IN (SELECT d.b_id FROM d WHERE d.id = a.id)")
	})

	// --- Controls: non-subquery compound ON clauses still work correctly.

	t.Run("ctrl_constant_conjunct", func(t *testing.T) {
		rows, err := db.QueryContext(ctx, "SELECT a.id, c.id FROM a JOIN b ON b.a_id = a.id "+
			"LEFT JOIN c ON c.a_id = a.id AND c.w = 12345")
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		got := siScanRows(t, rows)
		want := []string{"1|NULL", "2|NULL"}
		if !eqStrSlices(got, want) {
			t.Errorf("constant-conjunct LEFT JOIN rows = %v, want %v", got, want)
		}
	})
	t.Run("ctrl_single_eq", func(t *testing.T) {
		rows, err := db.QueryContext(ctx, "SELECT a.id, c.id FROM a JOIN b ON b.a_id = a.id LEFT JOIN c ON c.a_id = a.id")
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		got := siScanRows(t, rows)
		want := []string{"1|50", "2|51"}
		if !eqStrSlices(got, want) {
			t.Errorf("single-eq LEFT JOIN rows = %v, want %v", got, want)
		}
	})
	// IN with a value LIST (not a subquery) must still work — the detector must
	// not over-reject `IN (a, b, c)`.
	t.Run("ctrl_in_value_list", func(t *testing.T) {
		rows, err := db.QueryContext(ctx, "SELECT a.id, c.id FROM a JOIN b ON b.a_id = a.id "+
			"LEFT JOIN c ON c.a_id = a.id AND c.w IN (999, 888)")
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		got := siScanRows(t, rows)
		want := []string{"1|50", "2|51"}
		if !eqStrSlices(got, want) {
			t.Errorf("IN-value-list LEFT JOIN rows = %v, want %v", got, want)
		}
	})
}

func eqStrSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
