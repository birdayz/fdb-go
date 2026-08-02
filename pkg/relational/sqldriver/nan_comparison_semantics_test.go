package sqldriver_test

// Pins what NaN comparison ACTUALLY does today, because it was documented as
// doing the opposite.
//
// DIVERGENCES.md claimed Go returned FALSE for `NaN = NaN` "(IEEE, SQL
// standard)" while Java returned TRUE. That was asserted and never measured.
// Go returns TRUE. That matches Java's boxed SimpleComparison path,
// PostgreSQL, and CockroachDB's canonical NaN equality; it differs from IEEE
// primitive `==` and Java's direct RelOpValue EQ_DD/EQ_FF path.
//
// The behaviour is deliberate: predicate comparison falls through to
// values.CompareFloat64, the Double.compare total order with NaN greatest, and
// that comment says so. PostgreSQL and Java's boxed ordering also put NaN
// greatest; CockroachDB canonicalizes NaNs but deliberately sorts them first.
// FDB tuple order is a separate physical order: it preserves NaN sign/payload,
// so negative NaNs sort below -Inf and positive NaNs above +Inf. Indexed
// ordered float predicates therefore decline until that mismatch has an exact
// physical access shape.
//
// This test therefore pins CURRENT behaviour rather than desired behaviour, and
// exists so the semantics cannot drift unobserved while the question is open.
// If predicate comparison is ever moved to IEEE, this file must be updated
// deliberately — which is the point.
//
// NaN has no SQL literal spelling, so it can only arise from a computed
// expression (0.0/0.0).

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"testing"
)

func TestFDB_NaNComparisonSemantics(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_nansem")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_nansem")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE nansem "+
		"CREATE TABLE t (id BIGINT, v DOUBLE, z DOUBLE, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_nansem/s WITH TEMPLATE nansem")
	dsn := fmt.Sprintf("fdbsql:///testdb_nansem?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// z = 0 everywhere: v/z is NaN for rows 1-2 (0.0/0.0) and +Inf for row 3.
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, v, z) VALUES (1, 0.0, 0.0), (2, 0.0, 0.0), (3, 5.0, 0.0)")

	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("db.Conn: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	ids := func(t *testing.T, q string) []int64 {
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
		sort.Slice(o, func(i, j int) bool { return o[i] < o[j] })
		return o
	}
	eq := func(g, w []int64) bool {
		if len(g) != len(w) {
			return false
		}
		for i := range g {
			if g[i] != w[i] {
				return false
			}
		}
		return true
	}

	for _, tc := range []struct {
		query string
		want  []int64
		note  string
	}{
		{
			"SELECT id FROM t WHERE (v / z) = (v / z)",
			[]int64{1, 2, 3},
			"NaN = NaN is TRUE here (boxed-Java/PostgreSQL/Cockroach-compatible). " +
				"Primitive IEEE and Java's direct RelOp path say FALSE, which would make this [3] only",
		},
		{
			"SELECT id FROM t WHERE (v / z) <> (v / z)", nil,
			"the mirror: primitive IEEE/Java RelOp would make this [1 2]",
		},
		{
			"SELECT id FROM t WHERE (v / z) > 0",
			[]int64{1, 2, 3},
			"NaN is greatest in this total order, so it compares above 0. Primitive IEEE " +
				"comparisons are false, which would make this [3] only",
		},
		{
			"SELECT id FROM t WHERE (v / z) < 0", nil,
			"consistent under both readings",
		},
		{
			// Dedup/grouping is settled independently of the raw tuple codec:
			// DISTINCT recursively canonicalizes NaN sign/payload variants in
			// its private key, while continuations and stored FDB keys retain
			// their exact bits.
			"SELECT id FROM t WHERE (v / z) >= 0",
			[]int64{1, 2, 3},
			"same total-order reasoning as `> 0`",
		},
	} {
		t.Run(tc.query, func(t *testing.T) {
			if got := ids(t, tc.query); !eq(got, tc.want) {
				t.Fatalf("%s\n%s = %v, want %v — this test pins CURRENT behaviour; if you "+
					"changed NaN comparison deliberately, update it and DIVERGENCES.md together",
					tc.note, tc.query, got, tc.want)
			}
		})
	}

	// Two NaNs are ONE value for set operations through explicit logical
	// canonicalization, independent of raw FDB tuple-key identity.
	var n int64
	if err := conn.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM (SELECT DISTINCT v / z AS k FROM t) AS s").Scan(&n); err != nil {
		t.Fatalf("distinct count: %v", err)
	}
	if n != 2 {
		t.Fatalf("DISTINCT over {NaN, NaN, +Inf} = %d groups, want 2 — DISTINCT must "+
			"canonicalize NaN sign/payload variants as one logical value", n)
	}
}
