package sqldriver_test

// An IN list may hold a COLUMN, and then it is a RUNTIME list.
//
// `b IN (a, 999)` asks whether b equals a — per row — or 999. Java plans and
// answers it: visitInList sends an all-constant list through its literal
// pipeline and everything else through resolveFunction("__internal_array", …)
// (ExpressionVisitor.java:646-656), so a non-constant list is an ordinary array
// compared row by row. Go folded the list at PLAN time unconditionally, and
// ResolveIn returned "element N is not constant", which surfaced as
// 0AF00 "Cascades planner could not plan query" — measured against the live JVM
// in conformance/in_list_shapes_java_probe_test.go, where Java answered [[1] [3]].
//
// The runtime form needed no new machinery: ArrayConstructorValue.Evaluate
// returns the []any the IN comparison already expects, and
// ComparisonPredicate.Eval evaluates the RHS against the SAME row context as
// the LHS, so a column-referencing element reads the current row.
//
// THE DANGEROUS HALF IS THE PLANNER, NOT THE RESOLVER, and it is why this file
// asserts plan SHAPE as well as rows. InComparisonToExplodeRule folds the IN
// list at plan time with Operand.Evaluate(nil) and explodes over the result.
// fieldValue.Evaluate(nil) answers (nil, nil) — NOT an error — so an unguarded
// fold turns `b IN (a, 999)` into an explode over [NULL, 999]: the query plans,
// runs, and silently returns the rows of `b IN (NULL, 999)`. Making the
// resolver accept the shape without hardening that guard would have shipped
// wrong rows rather than an error, which is strictly worse than the gap it
// closes. The guard now tests IsConstantValue, which is recursive over children
// and cannot be fooled by a composite holding a column reference.
//
// The constant arms below are the other half of that: the guard must decline
// ONLY the non-constant case. A constant IN list must still reach the InJoin /
// InUnion path, because that is where an IN list becomes index sub-probes, and
// a guard that over-declined would quietly turn every indexed IN query into a
// full scan while every row assertion still passed.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// openParenDB builds the two-table fixture the join arm needs. It is separate
// from the twin above because the join shape has nothing to do with indexes —
// what it measures is whether the ON predicate is APPLIED, and a second schema
// would only double the setup.
func openParenDB(t *testing.T) *sql.DB {
	t.Helper()
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_in_join")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_in_join")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE injoin_t "+
		"CREATE TABLE l (id BIGINT, x BIGINT, PRIMARY KEY (id)) "+
		"CREATE TABLE r (id BIGINT, y BIGINT, lo BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_in_join/s WITH TEMPLATE injoin_t")
	db, err := sql.Open("fdbsql",
		fmt.Sprintf("fdbsql:///testdb_in_join?cluster_file=%s&schema=s", clusterFilePath))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestFDB_InListWithNonConstantItems(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	w := mmNewTwin(t, ctx, "/testdb_in_nonconst", "innonconst",
		"CREATE TABLE t (id BIGINT, a BIGINT, b BIGINT, s STRING, PRIMARY KEY (id)) ",
		"CREATE INDEX t_b ON t (b) CREATE INDEX t_a ON t (a) ")
	//  id=1: a=10 b=10   a == b
	//  id=2: a=7  b=20   a != b, b is in a constant list below
	//  id=3: a=30 b=30   a == b
	//  id=4: a=NULL b=40 a IS NULL — the 3VL row
	w.Exec("INSERT INTO t (id, a, b, s) VALUES " +
		"(1, 10, 10, 'x'), (2, 7, 20, 'y'), (3, 30, 30, 'z'), (4, NULL, 40, 'w')")

	t.Run("rows", func(t *testing.T) {
		w := w.Sub(t)
		cases := []struct {
			name, pred string
			want       []string
		}{
			{"column among constants", "b IN (a, 999)", []string{"1", "3"}},
			{"column as the only item", "b IN (a)", []string{"1", "3"}},
			{"column plus a matching constant", "b IN (a, 20)", []string{"1", "2", "3"}},
			{"arithmetic over a column", "b IN (a + 0, 999)", []string{"1", "3"}},
			// The offset is chosen so the arithmetic changes WHICH row matches
			// rather than merely preserving the a == b rows: only id=2 has
			// b == a + 13 (20 == 7 + 13). If the item were evaluated once at
			// plan time instead of per row, no row could match this.
			{"arithmetic that shifts the match", "b IN (a + 13, 999)", []string{"2"}},
			{"two column items", "b IN (a, id)", []string{"1", "3"}},
			// NOT IN over a list holding a NULL-valued column: row 4 has a NULL
			// item, so its membership is UNKNOWN and NOT IN must not return it.
			// Rows 1 and 3 match so they are out; row 2 does not match and has
			// no NULL, so it is the only answer.
			{"negated", "b NOT IN (a, 999)", []string{"2"}},
			// Composes with the one-field-record flatten: both the left operand
			// and the items are parenthesized.
			{"parenthesized operands", "(b) IN ((a), 999)", []string{"1", "3"}},
			{"string column item", "s IN (s, 'nope')", []string{"1", "2", "3", "4"}},
		}
		for _, c := range cases {
			w.Want(c.name,
				fmt.Sprintf("SELECT id FROM t WHERE %s ORDER BY id", c.pred), c.want)
		}
	})

	// A non-constant list is a RESIDUAL FILTER. It cannot be index sub-probes:
	// the values are not known until the row is read.
	t.Run("plan_shape_non_constant_is_residual", func(t *testing.T) {
		for _, q := range []string{
			"SELECT id FROM t WHERE b IN (a, 999) ORDER BY id",
			"SELECT id FROM t WHERE b IN (a) ORDER BY id",
		} {
			plan := w.Explain(q)
			for _, forbidden := range []string{"InJoin", "InUnion"} {
				if strings.Contains(plan, forbidden) {
					t.Errorf("a NON-constant IN list reached the %s path. Its values are not known "+
						"until the row is read, so folding them at plan time explodes over whatever "+
						"Evaluate(nil) returned — (nil, nil) for a column reference — and silently "+
						"answers a different query.\n  q: %s\n  plan: %s", forbidden, q, plan)
				}
			}
		}
	})

	// ...and a CONSTANT list must still take it. This is the over-decline
	// guard: without it the hardening could turn every indexed IN query into a
	// full scan and every row assertion above would still pass.
	t.Run("plan_shape_constant_still_probes_the_index", func(t *testing.T) {
		const q = "SELECT id FROM t WHERE b IN (10, 30) ORDER BY id"
		plan := w.Explain(q)
		if !strings.Contains(plan, "InJoin") && !strings.Contains(plan, "InUnion") {
			t.Errorf("a CONSTANT IN list no longer reaches the InJoin/InUnion path, so the "+
				"non-constant guard is declining too much and indexed IN queries have quietly "+
				"become scans.\n  q: %s\n  plan: %s", q, plan)
		}
		w.Want("constant list still correct", q, []string{"1", "3"})
	})

	// A CROSS-TABLE IN list in a JOIN ON clause. This shape has a history and
	// it is the reason this arm exists rather than being folded into the rows
	// group above: it used to be dropped silently from the ON clause, turning
	// the join into a CROSS PRODUCT, and was then made to fail closed with a
	// clean error. Making it plannable again re-enters exactly that territory,
	// so the row count is asserted against the join's full cross product to say
	// the predicate is being APPLIED and not merely accepted.
	t.Run("cross_table_in_a_join_ON", func(t *testing.T) {
		db := openParenDB(t)
		mwjoMustExec(t, db, ctx, "INSERT INTO l (id, x) VALUES (1, 5), (2, 10), (3, 7)")
		mwjoMustExec(t, db, ctx, "INSERT INTO r (id, y, lo) VALUES (50, 5, 1), (51, 99, 8)")

		// Only l=1 (x=5) satisfies `x IN (y, lo)` — against r=50, where y=5.
		// The full cross product is 3 x 2 = 6 rows, so a 6-row answer is the
		// old silent drop returning and a 0-row answer is the predicate being
		// evaluated against the wrong row.
		got, err := mmRows(t, ctx, db,
			"SELECT l.id, r.id FROM l JOIN r ON l.x IN (r.y, r.lo) ORDER BY l.id, r.id")
		if err != nil {
			t.Fatalf("a cross-table IN in an ON clause failed to plan: %v", err)
		}
		want := []string{"1|50"}
		if !mmEqRows(got, want) {
			t.Fatalf("cross-table IN in an ON clause is wrong\n  got  %v\n  want %v\n"+
				"  (6 rows would be the join's full cross product — the predicate dropped; "+
				"0 rows would be it evaluated against the wrong row)\n  %s",
				got, want, mmFirstDiff(got, want))
		}

		// The same predicate in WHERE must agree with it in ON — the two
		// spellings of an inner join's filter cannot disagree.
		gotWhere, err := mmRows(t, ctx, db,
			"SELECT l.id, r.id FROM l, r WHERE l.x IN (r.y, r.lo) ORDER BY l.id, r.id")
		if err != nil {
			t.Fatalf("the WHERE spelling failed to plan: %v", err)
		}
		if !mmEqRows(gotWhere, want) {
			t.Fatalf("the ON and WHERE spellings of the same cross-table IN disagree\n"+
				"  ON   : %v\n  WHERE: %v", got, gotWhere)
		}
	})

	// The rejections that must survive. A non-constant list must not become a
	// back door around the checks the constant path applies.
	t.Run("still_rejected", func(t *testing.T) {
		w := w.Sub(t)
		// A NULL among the items is rejected whether or not the list is
		// constant — the check runs on the resolved item, before the constant
		// fork.
		w.WantRejected("NULL beside a column item",
			"SELECT id FROM t WHERE b IN (a, NULL) ORDER BY id", "42809")
		// An item whose type cannot unify with the LHS is a type error on both
		// forks; the gate is type-based and runs before the fork.
		w.WantRejected("incompatible item type beside a column item",
			"SELECT id FROM t WHERE b IN (a, 'x') ORDER BY id", "42804")
	})
}
