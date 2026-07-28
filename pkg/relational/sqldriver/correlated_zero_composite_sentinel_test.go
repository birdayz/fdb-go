package sqldriver_test

// KNOWN BUG sentinel — the correlated half of CQ-28, measured rather than
// assumed. Fails loudly the day it is fixed, forcing this file to be updated.
//
// CQ-28 fixed a zero-valued float equality in a NON-terminal composite
// position by terminating the scan prefix at match time, so the executor widens
// the bound across both signed zeros and the trailing predicate becomes a
// residual filter. That detection needs the comparand's VALUE, which at match
// time exists only for a compile-time constant.
//
// A CORRELATED comparand — supplied per-probe by an outer row — is opaque at
// match time. The full composite prefix is kept, no widening applies, and a
// stored -0.0 is missed when the outer supplies +0.0 (or vice versa). Measured:
//
//	t.v = o.k AND t.w = 5   (o.k = +0.0, t.v = -0.0)  -> []   WRONG, want [1]
//	  plan: FlatMap(Scan(O), Fetch(IndexScan(T_VW, [=, =])))
//	t.v = o.k               (same values, terminal)   -> [1]  correct
//	  plan: FlatMap(Scan(O), Fetch(IndexScan(T_VW, [=, *])))
//	v = 0 AND w = 5         (constant, CQ-28 path)    -> [1]  correct
//	  plan: PredicatesFilter(IndexScan(T_VW, [=, *]))
//
// Why it is not fixed the same way: the only match-time option is to terminate
// the prefix at EVERY float equality whose comparand is opaque, which de-sargs
// every correlated composite join on a float leading column — trading a rare
// wrong row for a broad performance cliff on a common shape. Fixing it properly
// means widening plus residual filtering decided at EXECUTION time, where the
// comparand's value is finally known, which changes the index scan's contract
// (it would return more rows than its range implies and filter internally).
// That is a real design change and belongs behind its own gate.
//
// PASSES today by asserting the CURRENT (buggy) result. See RFC-196.

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestFDB_CorrelatedZeroCompositeSentinel(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_czs")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_czs")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE czs "+
		"CREATE TABLE t (id BIGINT NOT NULL, v DOUBLE, w BIGINT, PRIMARY KEY (id)) "+
		"CREATE INDEX t_vw ON t (v, w) "+
		"CREATE TABLE o (id BIGINT NOT NULL, k DOUBLE, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_czs/s WITH TEMPLATE czs")
	dsn := fmt.Sprintf("fdbsql:///testdb_czs?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// t.v is NEGATIVE zero; o.k is POSITIVE zero. IEEE says they are equal.
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, v, w) VALUES (1, -0.0, 5), (2, 5.0, 5)")
	mwjoMustExec(t, db, ctx, "INSERT INTO o (id, k) VALUES (10, 0.0), (20, 5.0)")

	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("db.Conn: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	count := func(t *testing.T, q string) int {
		t.Helper()
		rows, err := conn.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		n := 0
		for rows.Next() {
			n++
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}
		return n
	}

	const buggy = "SELECT t.id FROM t, o WHERE t.v = o.k AND t.w = 5 AND o.id = 10"
	if n := count(t, buggy); n != 0 {
		t.Fatalf("%q now returns %d rows — the correlated half of CQ-28 may be FIXED. "+
			"Verify it returns exactly t.id 1, then flip this sentinel to assert that and "+
			"close the gap in RFC-196", buggy, n)
	}

	// The three shapes that MUST keep working, so a "fix" that simply
	// de-sargs everything cannot pass by accident.
	for _, tc := range []struct {
		query string
		want  int
		why   string
	}{
		{
			"SELECT t.id FROM t, o WHERE t.v = o.k AND o.id = 10", 1,
			"correlated zero in TERMINAL position still widens correctly",
		},
		{
			"SELECT id FROM t WHERE v = 0 AND w = 5", 1,
			"the constant-zero composite case CQ-28 fixed must stay fixed",
		},
		{
			"SELECT t.id FROM t, o WHERE t.v = o.k AND t.w = 5 AND o.id = 20", 1,
			"a correlated NONZERO comparand keeps the full composite prefix and matches",
		},
	} {
		if n := count(t, tc.query); n != tc.want {
			t.Fatalf("%s\n%q returned %d rows, want %d", tc.why, tc.query, n, tc.want)
		}
	}
}
