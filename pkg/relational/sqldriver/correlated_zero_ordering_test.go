package sqldriver_test

// A CORRELATED float operand that binds to zero widens the scan just like a
// literal zero does, so the suffix ordering claim must be dropped for it too.
//
// The first ordering fix only recognised a compile-time-constant zero. A
// correlated operand — `t.v = o.k` with o.k a DOUBLE column — is opaque at plan
// time, so the equality was still counted as pinning one key and the suffix was
// advertised as ordered. Measured, over rows (-0.0, 9) and (+0.0, 1):
//
//	ORDER BY t.w          -> [9 1]  unsorted, no sort node in the plan
//	ORDER BY t.w LIMIT 1  -> [9]    wrong row
//
// A non-constant operand whose DECLARED type is float is now treated
// conservatively: it might bind to zero, so nothing after it claims an order.
// That costs a sort and nothing else.
//
// Deliberately asymmetric with the SARGABILITY decision for the same shape
// (see RFC-196): there, treating a correlated float conservatively de-sargs the
// probe into a scan of the whole leading-column group — a performance cliff on
// every correlated composite join — so the gap is left open and pinned instead.
// Here the conservative answer costs one sort. Different price, different call.
//
// KNOWN HOLE: an operand that reaches the plan gates UNTYPED is not covered,
// because treating unknown as "could be float" swallows every untyped IN-join
// binding over an INT column and costs those plans their ordering claim — it
// turned four test targets red, including an InJoin that must claim ascending
// order. The right discriminator is the INDEXED COLUMN's type, which ordering
// computation cannot currently see. Bound `?` parameters do NOT hit this: they
// resolve before planning, verified separately.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

func TestFDB_CorrelatedZeroOrdering(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_czo")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_czo")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE czo "+
		"CREATE TABLE t (id BIGINT NOT NULL, v DOUBLE, w BIGINT, PRIMARY KEY (id)) "+
		"CREATE INDEX t_vw ON t (v, w) "+
		"CREATE TABLE o (id BIGINT NOT NULL, k DOUBLE, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_czo/s WITH TEMPLATE czo")
	dsn := fmt.Sprintf("fdbsql:///testdb_czo?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// Physical scan order is (-0.0, 9) then (+0.0, 1): w DESCENDS across the
	// signed-zero boundary, so any claim that w is ordered is observably false.
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, v, w) VALUES (1, -0.0, 9), (2, 0.0, 1)")
	mwjoMustExec(t, db, ctx, "INSERT INTO o (id, k) VALUES (10, 0.0)")

	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("db.Conn: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	vals := func(t *testing.T, q string) []int64 {
		t.Helper()
		rows, err := conn.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		var o []int64
		for rows.Next() {
			var v int64
			if err := rows.Scan(&v); err != nil {
				t.Fatalf("scan: %v", err)
			}
			o = append(o, v)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}
		return o // order IS the assertion
	}

	for _, tc := range []struct {
		query string
		want  []int64
	}{
		{"SELECT t.w FROM t, o WHERE t.v = o.k AND o.id = 10 ORDER BY t.w", []int64{1, 9}},
		{"SELECT t.w FROM t, o WHERE t.v = o.k AND o.id = 10 ORDER BY t.w LIMIT 1", []int64{1}},
		{"SELECT t.w FROM t, o WHERE t.v = o.k AND o.id = 10 ORDER BY t.w DESC LIMIT 1", []int64{9}},
	} {
		t.Run(tc.query, func(t *testing.T) {
			if plan := explainOnConn(t, ctx, conn, tc.query); !strings.Contains(plan, "Sort") {
				t.Fatalf("plan = %s\nwant a sort node: the correlated operand may bind to zero, "+
					"which widens the scan across two prefixes, so w's order is not provided",
					plan)
			}
			got := vals(t, tc.query)
			if len(got) != len(tc.want) {
				t.Fatalf("%s = %v, want %v", tc.query, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("%s = %v, want %v — w resets at the signed-zero boundary, so an "+
						"elided sort yields index order", tc.query, got, tc.want)
				}
			}
		})
	}
}
