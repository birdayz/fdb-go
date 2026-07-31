package sqldriver_test

// Regression for `(<comparison>) IS NULL`.
//
// The predicate asks whether a comparison evaluated to UNKNOWN, which is the
// only way SQL can ask that question and therefore the third branch of every
// ternary-logic partition. Go rejected it with 0AF00 ("Cascades translation
// failed") while accepting every neighbouring shape — `(a > 3 AND b > 1) IS
// NULL`, `(NOT (a > 3)) IS NULL`, `(a IS NULL) IS NULL` all planned — because
// a bare comparison inside a paren-wrap resolved through the walk that
// suppresses comparisons (the suppression exists for CASE WHEN ... THEN, a
// different position with a different Java answer).
//
// Java accepts it: comparisons there are Values (RelOpValue), the one-field
// record flatten removes the parentheses, and IsNullFn takes Type.Any, so the
// operand reaches Comparisons.Type.IS_NULL as a boolean-typed Value.
//
// Planning is not the assertion. A predicate that plans and returns the wrong
// rows is worse than one that refuses, so this pins the ROWS for all three
// three-valued outcomes and, separately, the partition property that makes the
// predicate worth having.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"testing"
)

func TestFDB_IsNullOverComparison(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_isnullcmp")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_isnullcmp")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE isnullcmp "+
			"CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, b BIGINT NOT NULL, s STRING, PRIMARY KEY (id)) "+
			"CREATE INDEX t_a ON t (a)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_isnullcmp/s WITH TEMPLATE isnullcmp")
	dsn := fmt.Sprintf("fdbsql:///testdb_isnullcmp?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// a: 1, 5, NULL, 9, NULL   → `a > 3` is FALSE, TRUE, UNKNOWN, TRUE, UNKNOWN
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, a, b, s) VALUES "+
		"(1, 1, 10, 'x'), (2, 5, 20, 'y'), (3, NULL, 30, NULL), (4, 9, 40, 'z'), (5, NULL, 50, 'w')")

	ids := func(q string) []int64 {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		var out []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				t.Fatalf("scan %q: %v", q, err)
			}
			out = append(out, id)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows %q: %v", q, err)
		}
		sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
		return out
	}

	eq := func(name string, got, want []int64) {
		t.Helper()
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s: got %v, want %v", name, got, want)
		}
	}

	// The three-valued split of `a > 3` over the seeded rows.
	eq("p", ids("SELECT id FROM t WHERE (a > 3)"), []int64{2, 4})
	eq("not-p", ids("SELECT id FROM t WHERE NOT (a > 3)"), []int64{1})
	eq("p-is-null", ids("SELECT id FROM t WHERE (a > 3) IS NULL"), []int64{3, 5})
	eq("p-is-not-null", ids("SELECT id FROM t WHERE (a > 3) IS NOT NULL"), []int64{1, 2, 4})

	// The partition property the predicate exists to make expressible: the
	// three branches reconstruct the unfiltered table exactly, with no row in
	// two branches and none missing. Asserting the branches individually would
	// pass a NULL-semantics bug that shifted a row from one branch to another
	// in a way the hand-written expectations happened to encode.
	var union []int64
	union = append(union, ids("SELECT id FROM t WHERE (a > 3)")...)
	union = append(union, ids("SELECT id FROM t WHERE NOT (a > 3)")...)
	union = append(union, ids("SELECT id FROM t WHERE (a > 3) IS NULL")...)
	sort.Slice(union, func(i, j int) bool { return union[i] < union[j] })
	eq("tlp partition", union, ids("SELECT id FROM t"))

	// Operand shapes that already worked must keep working — the fix routes
	// the IS operand through a resolver of its own, and a regression there
	// would silently change what these mean rather than failing to plan.
	eq("and-tree", ids("SELECT id FROM t WHERE (a > 3 AND b > 1) IS NULL"), []int64{3, 5})
	eq("not-tree", ids("SELECT id FROM t WHERE (NOT (a > 3)) IS NULL"), []int64{3, 5})
	eq("nested-isnull", ids("SELECT id FROM t WHERE (a IS NULL) IS NULL"), nil)
	eq("bare-column", ids("SELECT id FROM t WHERE a IS NULL"), []int64{3, 5})

	// A NOT NULL column can never make the comparison UNKNOWN.
	eq("not-null-col", ids("SELECT id FROM t WHERE (b > 3) IS NULL"), nil)
	// A string comparison takes the same path.
	eq("string-cmp", ids("SELECT id FROM t WHERE (s = 'x') IS NULL"), []int64{3})

	// The fix must not have leaked comparison handling into the CASE WHEN THEN
	// position, which Java rejects and which the shared walk deliberately
	// suppresses. If this stops erroring, the suppression was widened rather
	// than sidestepped.
	if _, err := db.QueryContext(ctx, "SELECT CASE WHEN id = 1 THEN a > 3 ELSE FALSE END FROM t"); err == nil {
		t.Error("`CASE WHEN ... THEN a > 3` was accepted; the IS-operand fix must not relax the comparison " +
			"suppression that keeps Go aligned with Java's rejection in a THEN branch")
	}
}
