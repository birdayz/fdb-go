package sqldriver_test

// Signed zero is TWO values for every set-like operation: DISTINCT, GROUP BY,
// UNION dedup, unique-index distinctness, and ORDER BY. Value identity in this
// engine is TUPLE-KEY identity, and the tuple encoding is Java's, which
// preserves the IEEE sign bit — Java's own TupleOrderingTest asserts -0.0 and
// +0.0 pack to distinct, adjacent keys with -0.0 first. Java's scalar `=` uses
// the same contract (Comparisons.java:246 → Double.equals → doubleToLongBits),
// and its index probe agrees with its filter.
//
// An earlier revision canonicalized zero inside packedDedupKey so DISTINCT
// would instead agree with cmpAny's IEEE `=`. The principle behind that —
// DISTINCT is an equality concept, not an ordering one — is sound, but it moved
// the wrong side, and it made the same query return different rows depending on
// the plan:
//
//	SELECT DISTINCT d, a FROM t                 -> 2 rows   (hash dedup)
//	SELECT DISTINCT d, a FROM t ORDER BY d, a   -> 3 rows   (ordered dedup)
//
// over rows (-0.0,1) (-0.0,2) (+0.0,1). The ordered path dedups against the
// PREVIOUS row only — sound because equal keys are adjacent in index order —
// and index order keeps the sign, so canonicalizing the key alone broke the
// adjacency the path rests on and emitted (-0.0,1) and (+0.0,1) as two rows,
// duplicates under the very equality being asserted.
//
// GROUP BY could not have followed either: a maintained aggregate index stores
// each group as its own entry keyed by the grouping prefix, so two signed zeros
// are two physical entries Java also reads. Merging would mean writing key
// bytes Java does not, or reporting a different group count from the same
// index. Splitting needs no guard anywhere; merging needs one on every path and
// still ends in a divergence.
//
// The cost is real and deliberate: `WHERE v = 0` matches BOTH rows (cmpAny is
// IEEE, and the index range is widened to agree with it — see
// negative_zero_index_sarg_probe_test.go), while DISTINCT keeps them apart. In
// strict SQL those should agree. They cannot here, and Java is in the same
// state. CockroachDB merges only because it normalizes zero inside its own key
// encoder; that option is closed to a port whose encoder is Java's.

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"testing"
)

// The actual reproducer of the bug the revert fixed, and the shape the
// single-column test below CANNOT express.
//
// Adjacent-only dedup is sound because equal keys are adjacent in index order.
// With one dedup column the two signed zeros ARE adjacent, so the hash and
// ordered paths agree even with the canonicalization present — the
// single-column test catches it only by row count. The disagreement needs a
// float that is NOT the final dedup column, so a second column can separate
// two rows the canonicalization then declares equal:
//
//	index order:        (-0.0,1)  (-0.0,2)  (+0.0,1)
//	canonical keys:     ( 0.0,1)  ( 0.0,2)  ( 0.0,1)
//	                     ^^^^^^^^^^^^^^^^^^^^^^^^^^ equal, but NOT adjacent
//
// so the ordered path emits all three while the hash path emits two, and
// `SELECT DISTINCT d, a` answers differently depending on whether ORDER BY
// pushed it onto the ordered path. Packing verbatim makes all three rows
// genuinely distinct and both paths return 3.
func TestFDB_NegativeZeroDistinctMultiColumnPlanIndependence(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_nzmulti")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_nzmulti")
	// Index leads on the DOUBLE column so the ordered dedup is eligible: the
	// inner ordering (D, A) prefix-matches the dedup columns (D, A).
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE nzmulti "+
		"CREATE TABLE t (id BIGINT NOT NULL, d DOUBLE, a BIGINT, PRIMARY KEY (id)) "+
		"CREATE INDEX t_da ON t (d, a)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_nzmulti/s WITH TEMPLATE nzmulti")
	dsn := fmt.Sprintf("fdbsql:///testdb_nzmulti?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, d, a) VALUES (1, -0.0, 1), (2, -0.0, 2), (3, 0.0, 1)")

	count := func(t *testing.T, q string) int {
		t.Helper()
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		n := 0
		for rows.Next() {
			n++
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows %q: %v", q, err)
		}
		return n
	}

	const unordered = "SELECT DISTINCT d, a FROM t"
	const ordered = "SELECT DISTINCT d, a FROM t ORDER BY d, a"

	got, gotOrdered := count(t, unordered), count(t, ordered)
	if got != gotOrdered {
		t.Fatalf("%q = %d rows but %q = %d — the SAME query must not depend on whether the "+
			"planner picked the hash or the ordered dedup path", unordered, got, ordered, gotOrdered)
	}
	// All three rows are distinct: the two signed zeros are distinct tuple keys,
	// and rows 1 and 3 differ only in that sign.
	if got != 3 {
		t.Fatalf("%q = %d rows, want 3 — (-0.0,1) (-0.0,2) (+0.0,1) are three distinct keys",
			unordered, got)
	}
	// Reversing the column order takes the float out of the leading position,
	// which changes which path is eligible; the answer must not move.
	if n := count(t, "SELECT DISTINCT a, d FROM t"); n != 3 {
		t.Fatalf("SELECT DISTINCT a, d = %d rows, want 3 — column order must not change the answer", n)
	}
}

func TestFDB_NegativeZeroDistinctDedupProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_nzdedup")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_nzdedup")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE nzdedup "+
		"CREATE TABLE d (id BIGINT NOT NULL, v DOUBLE, PRIMARY KEY (id)) "+
		"CREATE TABLE f (id BIGINT NOT NULL, v FLOAT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_nzdedup/s WITH TEMPLATE nzdedup")
	dsn := fmt.Sprintf("fdbsql:///testdb_nzdedup?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// Both signed zeros, plus a duplicated nonzero control that proves ordinary
	// dedup still collapses — otherwise "3 rows" could mean dedup is simply off.
	mwjoMustExec(t, db, ctx, "INSERT INTO d (id, v) VALUES (1, -0.0), (2, 0.0), (3, 5.0), (4, 5.0)")
	mwjoMustExec(t, db, ctx, "INSERT INTO f (id, v) VALUES (1, -0.0), (2, 0.0), (3, 5.0), (4, 5.0)")

	// signbit distinguishes the two zeros; plain == cannot.
	fmtSigned := func(v float64) string {
		if v == 0 && math.Signbit(v) {
			return "-0"
		}
		return fmt.Sprintf("%v", v)
	}
	collect := func(t *testing.T, q string) []float64 {
		t.Helper()
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		var got []float64
		for rows.Next() {
			var v float64
			if err := rows.Scan(&v); err != nil {
				t.Fatalf("scan: %v", err)
			}
			got = append(got, v)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows %q: %v", q, err)
		}
		// Sort signed so -0.0 and +0.0 keep a stable, distinguishable order.
		sort.Slice(got, func(i, j int) bool {
			if got[i] == got[j] {
				return math.Signbit(got[i]) && !math.Signbit(got[j])
			}
			return got[i] < got[j]
		})
		return got
	}
	show := func(vs []float64) string {
		out := "["
		for i, v := range vs {
			if i > 0 {
				out += " "
			}
			out += fmtSigned(v)
		}
		return out + "]"
	}
	sameShape := func(a, b []float64) bool {
		if len(a) != len(b) {
			return false
		}
		for i := range a {
			if a[i] != b[i] || math.Signbit(a[i]) != math.Signbit(b[i]) {
				return false
			}
		}
		return true
	}

	for _, tbl := range []string{"d", "f"} {
		t.Run(tbl, func(t *testing.T) {
			// -0.0, +0.0 and 5.0 — three values, with 5.0 deduped from two rows.
			want := []float64{math.Copysign(0, -1), 0, 5}

			distinct := collect(t, fmt.Sprintf("SELECT DISTINCT v FROM %s", tbl))
			if !sameShape(distinct, want) {
				t.Fatalf("%s: SELECT DISTINCT v = %s, want %s — the two signed zeros are "+
					"distinct tuple keys and must dedup as two values (and 5.0 must still collapse)",
					tbl, show(distinct), show(want))
			}

			// The property that the canonicalization broke: the ORDERED dedup
			// path (adjacent-only) must agree with the hash path. These are the
			// same query; only the plan differs.
			ordered := collect(t, fmt.Sprintf("SELECT DISTINCT v FROM %s ORDER BY v", tbl))
			if !sameShape(ordered, distinct) {
				t.Fatalf("%s: DISTINCT with ORDER BY = %s but without = %s — the same query must "+
					"not depend on which dedup path the planner picks",
					tbl, show(ordered), show(distinct))
			}

			// DISTINCT v and GROUP BY v ask the same question and must answer
			// alike; GROUP BY is pinned to split by the aggregate-index wire
			// format, so DISTINCT follows it rather than the other way around.
			grouped := collect(t, fmt.Sprintf("SELECT v FROM %s GROUP BY v", tbl))
			if !sameShape(grouped, distinct) {
				t.Fatalf("%s: GROUP BY v = %s but SELECT DISTINCT v = %s — these are the same "+
					"question and must not disagree", tbl, show(grouped), show(distinct))
			}
		})
	}
}
