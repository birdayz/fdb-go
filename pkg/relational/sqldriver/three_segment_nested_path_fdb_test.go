package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// A three-segment reference is `alias . struct column . member` — `a.n.sk`.
// Java answers it; Go refused it with 42703 "column reference with qualifier
// \"A.N\" cannot be resolved", measured live against the 4.12.11.0 conformance
// server by conformance/nested_groupby_key_java_probe_test.go's
// three_segment_select_control row (Java returned [[1] [1] [2]]).
//
// WHERE THE REFUSAL CAME FROM, measured by instrumenting the mint sites rather
// than read off the code: not an arity cap in the expression walker, which the
// SELECT list never reaches. splitColumnRef captured the reference as a
// (bare, qualifier, qualified) triple whose qualifier is the leading segments
// JOINED — "A.N" — and every resolution channel then asked the scope for a
// source or struct column of that name. Nothing is called "A.N", so
// mapColumnResolveError turned a SourceNotFoundError into UNDEFINED_COLUMN.
// The joined form cannot say where one segment ends and the next begins, which
// is why the fix carries the SEGMENTS and not a longer string.
//
// Java has no arity cap anywhere on this path: `fullId : uid (DOT uid)*` in the
// grammar, Identifier carries `name` plus a `List<String> qualifier` built
// segment by segment (IdentifierVisitor.java:56-64), and lookupNestedField
// consumes a matched PREFIX and walks whatever remains through an unbounded
// accessor loop (SemanticAnalyzer.java:559-601) before fusing the descent onto
// the root read with ofFieldsAndFuseIfPossible (SemanticAnalyzer.java:598-599).
//
// This test drives every clause, because the refusal was not one gate. Each
// clause reached the flattened qualifier through its own carrier, and three of
// them failed with three DIFFERENT symptoms — 42703 in SELECT/GROUP BY, a bare
// planner decline in WHERE, and an executor "malformed plan" in ORDER BY. A
// test that pinned only the SELECT list would have left the other two carriers
// unthreaded and green.
func TestFDB_ThreeSegmentNestedPathResolvesInEveryClause(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_3seg")
	mustExec(t, setup, ctx, "CREATE DATABASE /testdb_3seg")
	mustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE seg3_tmpl "+
		"CREATE TYPE AS STRUCT gst (sk BIGINT, co BIGINT) "+
		"CREATE TABLE t(id BIGINT, n gst, PRIMARY KEY(id))")
	mustExec(t, setup, ctx, "CREATE SCHEMA /testdb_3seg/s WITH TEMPLATE seg3_tmpl")

	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///testdb_3seg?cluster_file=%s&schema=s", clusterFilePath))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// sk and co differ per row and neither is the primary key, so a read of the
	// wrong member — or of the struct root — produces a different column, not a
	// coincidentally equal one.
	mustExec(t, db, ctx, "INSERT INTO t VALUES (1, (1, 7)), (2, (1, 8)), (3, (2, 9))")

	queryInts := func(t *testing.T, q string) []int64 {
		t.Helper()
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		var out []int64
		for rows.Next() {
			var v int64
			if err := rows.Scan(&v); err != nil {
				t.Fatalf("scan %q: %v", q, err)
			}
			out = append(out, v)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows %q: %v", q, err)
		}
		return out
	}
	eq := func(t *testing.T, got, want []int64) {
		t.Helper()
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("rows = %v, want %v", got, want)
		}
	}

	// Every clause the reference can appear in, with the answer a correct read
	// of the MEMBER produces. The two-segment spelling of the same descent is
	// asserted beside each one: the three-segment form is not a new capability,
	// it is the same descent with the source named, and the pair is what says
	// the alias segment was peeled rather than folded into the lookup.
	t.Run("select_list", func(t *testing.T) {
		eq(t, queryInts(t, "SELECT n.sk FROM t"), []int64{1, 1, 2})
		eq(t, queryInts(t, "SELECT a.n.sk FROM t AS a"), []int64{1, 1, 2})
		eq(t, queryInts(t, "SELECT t.n.sk FROM t"), []int64{1, 1, 2})
		// The OTHER member through the same struct root: a fix that resolved
		// the root and stopped would return sk for both.
		eq(t, queryInts(t, "SELECT a.n.co FROM t AS a"), []int64{7, 8, 9})
	})

	t.Run("where", func(t *testing.T) {
		eq(t, queryInts(t, "SELECT id FROM t AS a WHERE a.n.sk = 1"), []int64{1, 2})
		eq(t, queryInts(t, "SELECT id FROM t AS a WHERE a.n.co = 9"), []int64{3})
	})

	t.Run("order_by", func(t *testing.T) {
		// co descends while sk ascends, so ordering by the wrong member — or by
		// the struct root — cannot produce this sequence by accident.
		eq(t, queryInts(t, "SELECT id FROM t AS a ORDER BY a.n.co DESC"), []int64{3, 2, 1})
		eq(t, queryInts(t, "SELECT id FROM t AS a ORDER BY a.n.co ASC"), []int64{1, 2, 3})
	})

	t.Run("aggregate_argument", func(t *testing.T) {
		eq(t, queryInts(t, "SELECT COUNT(a.n.sk) FROM t AS a"), []int64{3})
		eq(t, queryInts(t, "SELECT MAX(a.n.co) FROM t AS a"), []int64{9})
		eq(t, queryInts(t, "SELECT SUM(a.n.co) FROM t AS a"), []int64{24})
	})

	t.Run("having", func(t *testing.T) {
		eq(t, queryInts(t, "SELECT a.id FROM t AS a GROUP BY a.id HAVING MAX(a.n.co) > 7"), []int64{2, 3})
	})
}

// TestFDB_ThreeSegmentPathsOfTwoSourcesDoNotCollapse is the AMBIGUITY axis, and
// it is the assertion a fix that parses the leading segment and then discards
// it would fail while every single-source test above stayed green.
//
// With two sources both declaring a struct column `n`, the leading segment is
// the ONLY thing separating `a.n.sk` from `b.n.sk`. A resolver that dropped it
// would answer both off whichever source it happened to reach first.
//
// THE TWO SOURCES CARRY DIFFERENT DATA ON PURPOSE, and that is not incidental
// to the test — it is what makes it able to fail. A self-join paired on the
// primary key (`FROM t AS a, t AS b WHERE a.id = b.id`) puts IDENTICAL rows on
// both legs, so a collapse onto one leg produces exactly the numbers a correct
// resolution produces and the test passes with the bug fully present. Two
// distinct tables with disjoint values make the wrong leg observable.
//
// The bare spelling `n.sk` over the same FROM is the control: it names no
// source, so it IS ambiguous, and Java's per-attribute rule makes that 42702
// rather than a silent first-match (SemanticAnalyzer.java:433-437, "Ambiguous
// reference %s").
func TestFDB_ThreeSegmentPathsOfTwoSourcesDoNotCollapse(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_3seg_amb")
	mustExec(t, setup, ctx, "CREATE DATABASE /testdb_3seg_amb")
	mustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE seg3amb_tmpl "+
		"CREATE TYPE AS STRUCT gst (sk BIGINT, co BIGINT) "+
		"CREATE TABLE t1(id BIGINT, n gst, PRIMARY KEY(id)) "+
		"CREATE TABLE t2(id BIGINT, n gst, PRIMARY KEY(id))")
	mustExec(t, setup, ctx, "CREATE SCHEMA /testdb_3seg_amb/s WITH TEMPLATE seg3amb_tmpl")

	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///testdb_3seg_amb?cluster_file=%s&schema=s", clusterFilePath))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// Disjoint value ranges: every t1 member is a single digit, every t2 member
	// is in the hundreds. Any leg confusion is visible at a glance and cannot
	// coincide.
	mustExec(t, db, ctx, "INSERT INTO t1 VALUES (1, (1, 7)), (2, (2, 8))")
	mustExec(t, db, ctx, "INSERT INTO t2 VALUES (1, (100, 700)), (2, (200, 800))")

	const join = " FROM t1 AS a, t2 AS b WHERE a.id = b.id"

	rowsOf := func(t *testing.T, q string) []int64 {
		t.Helper()
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		var out []int64
		for rows.Next() {
			var v int64
			if err := rows.Scan(&v); err != nil {
				t.Fatalf("scan %q: %v", q, err)
			}
			out = append(out, v)
		}
		return out
	}

	// Same member name, different source: only the leading segment tells them
	// apart, and the value ranges say which one answered.
	if got, want := rowsOf(t, "SELECT a.n.sk"+join), []int64{1, 2}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("a.n.sk = %v, want %v — values in the hundreds mean it read t2", got, want)
	}
	if got, want := rowsOf(t, "SELECT b.n.sk"+join), []int64{100, 200}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("b.n.sk = %v, want %v — single digits mean it read t1", got, want)
	}

	// Both in ONE projection, the strongest form: a collapse makes the two
	// columns come from one source rather than merely swapping them.
	rows, err := db.QueryContext(ctx, "SELECT a.n.sk, b.n.sk"+join)
	if err != nil {
		t.Fatalf("two-reference projection: %v", err)
	}
	defer rows.Close()
	var pairs []string
	for rows.Next() {
		var l, r int64
		if err := rows.Scan(&l, &r); err != nil {
			t.Fatalf("scan: %v", err)
		}
		pairs = append(pairs, fmt.Sprintf("%d/%d", l, r))
	}
	if got, want := fmt.Sprint(pairs), "[1/100 2/200]"; got != want {
		t.Fatalf("a.n.sk, b.n.sk = %s, want %s — equal-magnitude columns mean one leg answered both", got, want)
	}

	// The other member through the same roots, so the descent is not merely
	// picking the struct's first field on both sides.
	rows2, err := db.QueryContext(ctx, "SELECT b.n.co, a.n.co"+join)
	if err != nil {
		t.Fatalf("reversed two-reference projection: %v", err)
	}
	defer rows2.Close()
	var pairs2 []string
	for rows2.Next() {
		var l, r int64
		if err := rows2.Scan(&l, &r); err != nil {
			t.Fatalf("scan: %v", err)
		}
		pairs2 = append(pairs2, fmt.Sprintf("%d/%d", l, r))
	}
	if got, want := fmt.Sprint(pairs2), "[700/7 800/8]"; got != want {
		t.Fatalf("b.n.co, a.n.co = %s, want %s", got, want)
	}

	// The control. Without a leading segment the reference names no source, so
	// it is genuinely ambiguous and must be REFUSED, never first-matched to the
	// left leg. This is what makes the assertions above a statement about the
	// qualifier rather than about which leg happens to come first.
	if _, err := db.QueryContext(ctx, "SELECT n.sk"+join); err == nil {
		t.Fatal("bare n.sk over two sources both declaring n was answered; " +
			"it names no source and must be refused as ambiguous, not first-matched")
	} else if !strings.Contains(err.Error(), "42702") {
		t.Fatalf("bare n.sk over two sources: got %v, want 42702 ambiguous reference", err)
	}
}

// TestFDB_ThreeSegmentNestedPathPlanShape pins the PLAN, not just the rows.
//
// Rows alone cannot tell a resolved descent from a fallback that happens to
// produce the same numbers, and this reference has a specific failure mode that
// is invisible in the output: resolving to the struct ROOT and reading the
// member later by name. The EXPLAIN must therefore show the same access path
// for the three-segment spelling as for the two-segment one — the alias segment
// is a QUALIFIER, and qualifying a reference does not change how it is read.
func TestFDB_ThreeSegmentNestedPathPlanShape(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_3seg_plan")
	mustExec(t, setup, ctx, "CREATE DATABASE /testdb_3seg_plan")
	mustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE seg3plan_tmpl "+
		"CREATE TYPE AS STRUCT gst (sk BIGINT, co BIGINT) "+
		"CREATE TABLE t(id BIGINT, n gst, PRIMARY KEY(id))")
	mustExec(t, setup, ctx, "CREATE SCHEMA /testdb_3seg_plan/s WITH TEMPLATE seg3plan_tmpl")

	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///testdb_3seg_plan?cluster_file=%s&schema=s", clusterFilePath))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mustExec(t, db, ctx, "INSERT INTO t VALUES (1, (1, 7))")

	explain := func(t *testing.T, q string) string {
		t.Helper()
		var plan string
		if err := db.QueryRowContext(ctx, "EXPLAIN "+q).Scan(&plan); err != nil {
			t.Fatalf("EXPLAIN %q: %v", q, err)
		}
		return plan
	}

	// The qualifier is not part of the read. `SELECT a.n.sk FROM t AS a` and
	// `SELECT n.sk FROM t` denote the same column of the same table, so their
	// plans must agree — a three-segment reference that planned differently
	// would be resolving through some other channel than the two-segment one.
	two := explain(t, "SELECT n.sk FROM t")
	three := explain(t, "SELECT a.n.sk FROM t AS a")
	if two != three {
		t.Fatalf("plan for the three-segment spelling differs from the two-segment one:\n  n.sk:   %s\n  a.n.sk: %s", two, three)
	}
	// And it must be a real scan, not an empty/degenerate plan that would make
	// the equality above hold vacuously.
	if !strings.Contains(strings.ToUpper(three), "SCAN") {
		t.Fatalf("EXPLAIN of the three-segment projection shows no scan, so the equality above proves nothing: %s", three)
	}
	t.Logf("three-segment projection plan: %s", three)

	// The predicate form: the descent must be IN the plan, so the filter is
	// evaluated as a nested read rather than as an unresolved name.
	pred := explain(t, "SELECT id FROM t AS a WHERE a.n.co = 7")
	predTwo := explain(t, "SELECT id FROM t WHERE n.co = 7")
	if pred != predTwo {
		t.Fatalf("plan for the three-segment predicate differs from the two-segment one:\n  n.co:   %s\n  a.n.co: %s", predTwo, pred)
	}
	t.Logf("three-segment predicate plan: %s", pred)
}

// TestFDB_ThreeSegmentGroupKeyGroupsLikeItsTwoSegmentTwin is the arm above's
// refusal pin, FLIPPED to the rows its own instruction named.
//
// THE PROPERTY IS THE AGREEMENT OF THE TWO SPELLINGS, and it survives the flip
// unchanged — only the thing they must agree on moved. Before the segment
// carrier reached the group-key path, the descent question was asked through a
// two-part (qualifier, name) form: for `a.n.sk` it was really "is there a source
// called A.N", the answer was no, and the two arities of ONE key disagreed about
// which LAYER refused them — 0AF00 for `n.sk`, undefined-column for `a.n.sk`.
// They now agree by answering, and the assertion is the group COUNT rather than
// the absence of an error, because a key that lost its descent groups by the
// struct ROOT and still returns rows.
func TestFDB_ThreeSegmentGroupKeyGroupsLikeItsTwoSegmentTwin(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_3seg_gb")
	mustExec(t, setup, ctx, "CREATE DATABASE /testdb_3seg_gb")
	mustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE seg3gb_tmpl "+
		"CREATE TYPE AS STRUCT gst (sk BIGINT, co BIGINT) "+
		"CREATE TABLE t(id BIGINT, n gst, PRIMARY KEY(id))")
	mustExec(t, setup, ctx, "CREATE SCHEMA /testdb_3seg_gb/s WITH TEMPLATE seg3gb_tmpl")

	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///testdb_3seg_gb?cluster_file=%s&schema=s", clusterFilePath))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mustExec(t, db, ctx, "INSERT INTO t VALUES (1, (1, 7)), (2, (1, 8)), (3, (2, 9))")

	// (sk, co) = (1,7) (1,8) (2,9): two distinct sk over three rows, so the
	// correct answer is 2 groups. Grouping by the struct ROOT would give 3
	// (every struct value is distinct), and dropping the key would give 1 —
	// the three outcomes are separated by the count alone.
	for _, tc := range []struct{ name, query string }{
		{"two_segment_projected", "SELECT n.sk, COUNT(*) FROM t GROUP BY n.sk"},
		{"two_segment_unprojected", "SELECT COUNT(*) FROM t GROUP BY n.sk"},
		{"three_segment_projected", "SELECT a.n.sk, COUNT(*) FROM t AS a GROUP BY a.n.sk"},
		{"three_segment_unprojected", "SELECT COUNT(*) FROM t AS a GROUP BY a.n.sk"},
		{"three_segment_table_qualified", "SELECT COUNT(*) FROM t GROUP BY t.n.sk"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := db.QueryContext(ctx, tc.query)
			if err != nil {
				t.Fatalf("nested grouping key %q was refused: %v", tc.query, err)
			}
			defer rows.Close()
			n := 0
			for rows.Next() {
				n++
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("%q: iterate: %v", tc.query, err)
			}
			if n != 2 {
				t.Fatalf("%q returned %d groups, want 2.\n"+
					"  THREE means the key grouped by the struct ROOT instead of the "+
					"member; ONE means the key was dropped from the grouping.",
					tc.query, n)
			}
		})
	}
}
