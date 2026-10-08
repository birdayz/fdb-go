package testkit

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
	"strings"
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

func ScanRowStrings(t *testing.T, rows *sql.Rows) []string {
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

// siRenderRow renders the row the cursor is currently positioned on, whatever
// its arity and column types, as "v1|v2|...|vN".
func SiRenderRow(t *testing.T, rows *sql.Rows) string {
	t.Helper()
	cols, err := rows.Columns()
	if err != nil {
		return fmt.Sprintf("<columns: %v>", err)
	}
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		return fmt.Sprintf("<scan: %v>", err)
	}
	parts := make([]string, len(vals))
	for i, v := range vals {
		parts[i] = fmt.Sprintf("%v", v)
	}
	return strings.Join(parts, "|")
}

// assertUnsupported runs q and asserts it fails cleanly with
// ErrCodeUnsupportedQuery (0AF00) — NOT a silently-wrong cross product, and
// for EXPLAIN not a rendered plan for a query the engine cannot run.
func AssertUnsupported(t *testing.T, db *sql.DB, ctx context.Context, q string) {
	t.Helper()
	rows, err := db.QueryContext(ctx, q)
	if err == nil {
		// Some drivers defer the error to the first Next()/Scan.
		defer rows.Close()
		if rows.Next() {
			// Render whatever shape came back — callers pass both data queries
			// (where a row means a silent cross product) and EXPLAIN (where a
			// row means a plan was rendered for a query that cannot run), and a
			// fixed 2-int scan would print NULL|NULL for the latter.
			t.Fatalf("expected clean rejection, but got a row back: first=%s", SiRenderRow(t, rows))
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

func EqualStrings(a, b []string) bool {
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
