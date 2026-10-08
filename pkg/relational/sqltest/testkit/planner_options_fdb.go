package testkit

// FDB integration tests for the planner options Java's relational
// PlannerConfiguration reads and Go can act on: DISABLED_PLANNER_RULES,
// DISABLE_PLANNER_REWRITING and PLAN_RIGHT_DEEP. They were defined on the Go
// option enum but nothing in the planner read them, so a user who set one got
// the full default rule set and no diagnostic. These tests drive each option
// through the whole stack — connection options → generator → Cascades planner →
// EXPLAIN and rows against a live store — because that is precisely the layer
// that used to drop them.
//
// Java reads a fourth, INDEX_FETCH_METHOD, which Go cannot honor (no
// remote-fetch implementation at any layer); it is untested here on purpose and
// tracked as its own TODO item rather than given a test that would assert
// nothing.

import (
	"context"
	"database/sql"
	"testing"
)

// explainOnConn returns the EXPLAIN plan text for q on the pinned connection,
// so the connection's api.Options are the ones in effect. EXPLAIN routes
// through the same planSelectCascades the real query path uses.
func ExplainConn(t *testing.T, ctx context.Context, conn *sql.Conn, q string) string {
	t.Helper()
	var plan string
	if err := conn.QueryRowContext(ctx, "EXPLAIN "+q).Scan(&plan); err != nil {
		t.Fatalf("EXPLAIN %s: %v", q, err)
	}
	return plan
}

// scanInt64Rows drains q into a slice of the first column's values.
func ScanInt64Rows(t *testing.T, ctx context.Context, conn *sql.Conn, q string) []int64 {
	t.Helper()
	rows, err := conn.QueryContext(ctx, q)
	if err != nil {
		t.Fatalf("query %s: %v", q, err)
	}
	defer func() { _ = rows.Close() }()
	var out []int64
	for rows.Next() {
		var v sql.NullInt64
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, v.Int64)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}
