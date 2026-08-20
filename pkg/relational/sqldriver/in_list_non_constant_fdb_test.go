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
func openParenDB(t *testing.T) *sql.DB { return openInJoinDB(t, "/testdb_in_join", "injoin_t") }

// openParenDB2 is the same fixture under a distinct database path. Each arm
// gets its own so the two can run in parallel without one's INSERTs being
// visible to the other's assertions — a shared path would make the row counts
// depend on scheduling.
func openParenDB2(t *testing.T) *sql.DB { return openInJoinDB(t, "/testdb_in_param", "inparam_t") }

// openUUIDInDB builds a single-table fixture pairing a UUID column with a
// STRING column, which is the only way to get a NON-CONSTANT item whose type
// needs converting before it can be compared with the left operand.
func openUUIDInDB(t *testing.T) *sql.DB {
	t.Helper()
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_in_uuid")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_in_uuid")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE inuuid_t "+
		"CREATE TABLE u (id BIGINT, uu UUID, us STRING, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_in_uuid/s WITH TEMPLATE inuuid_t")
	db, err := sql.Open("fdbsql",
		fmt.Sprintf("fdbsql:///testdb_in_uuid?cluster_file=%s&schema=s", clusterFilePath))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func openInJoinDB(t *testing.T, dbPath, template string) *sql.DB {
	t.Helper()
	ctx := context.Background()
	setup := openTestDB(t, dbPath)
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE "+dbPath)
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE "+template+" "+
		"CREATE TABLE l (id BIGINT, x BIGINT, PRIMARY KEY (id)) "+
		"CREATE TABLE r (id BIGINT, y BIGINT, lo BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA "+dbPath+"/s WITH TEMPLATE "+template)
	db, err := sql.Open("fdbsql",
		fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=s", dbPath, clusterFilePath))
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

	// A `?` PLACEHOLDER among the IN items, through the DRIVER — which is not
	// the same thing as a ParameterValue, and this arm was originally written
	// as though it were.
	//
	// The driver never plans a parameter at all. substituteParams "replaces
	// positional '?' placeholders in a query with SQL literal representations
	// of the supplied driver values" (embedded/utilities.go) BEFORE the parser
	// runs, so `x IN (?, 999)` reaches the engine as the constant list
	// `x IN (5, 999)`. It took the plan-time fold before the non-constant fork
	// existed and it still does; nothing about this path changed.
	//
	// So what this arm actually pins is the interpolated round trip: a
	// placeholder inside an IN list is substituted per EXECUTION, and two
	// executions with different bindings give different answers rather than one
	// cached plan's answer twice. That is worth having and it is what the
	// assertion below discriminates — it just is not the claim the arm first
	// carried.
	//
	// The ParameterValue claim is pinned where a ParameterValue exists:
	// pkg/relational/core/query/expr/in_list_parameter_walk_test.go drives the
	// walker directly, so the `?` survives to become one, and asserts both that
	// the list takes the runtime fork and that it evaluates against a binding.
	t.Run("a placeholder among the items, through the driver", func(t *testing.T) {
		pdb := openParenDB2(t)
		mwjoMustExec(t, pdb, ctx, "INSERT INTO l (id, x) VALUES (1, 5), (2, 10), (3, 7)")

		scanIDs := func(q string, args ...any) ([]string, error) {
			rows, err := pdb.QueryContext(ctx, q, args...)
			if err != nil {
				return nil, err
			}
			defer rows.Close()
			var out []string
			for rows.Next() {
				var id int64
				if err := rows.Scan(&id); err != nil {
					return nil, err
				}
				out = append(out, fmt.Sprint(id))
			}
			return out, rows.Err()
		}

		const q = "SELECT id FROM l WHERE x IN (?, 999) ORDER BY id"
		got5, err5 := scanIDs(q, int64(5))
		got10, err10 := scanIDs(q, int64(10))
		if err5 != nil || err10 != nil {
			t.Fatalf("a parameterized IN list failed to run\n  ?=5 : %v\n  ?=10: %v", err5, err10)
		}
		if !mmEqRows(got5, []string{"1"}) || !mmEqRows(got10, []string{"2"}) {
			t.Fatalf("a placeholder inside an IN list does not track its binding\n"+
				"  ?=5  got %v want [1]\n  ?=10 got %v want [2]\n"+
				"  (the SAME answer for both bindings means the substitution is not happening "+
				"per execution — a plan or a rendered statement is being reused across bindings)",
				got5, got10)
		}
	})

	// A UUID left operand with a NON-CONSTANT STRING item.
	//
	// This is the one coercion that does not survive the crossing to a runtime
	// list on its own. The constant fork converts each STRING element to the
	// [16]byte a UUID field evaluates as (parseStringToUUID); the runtime fork
	// returns before that loop. cmpAny has no [16]byte-versus-string arm, so
	// without a promotion the two sides are simply not comparable: equal values
	// are SILENTLY not matched, and NOT IN admits the rows it should exclude.
	//
	// Nothing about the shape looks wrong — the query plans, runs, and returns
	// a plausible answer — which is why both directions are asserted here. The
	// NOT IN arm is the one that would go unnoticed longest, because "more rows
	// than expected" from a negation reads as ordinary.
	//
	// The two numeric coercions the constant fork also applies are deliberately
	// NOT mirrored, and their absence is not a gap: they exist to make an index
	// sub-probe pack the right tuple type, a runtime list never drives one, and
	// cmpAny promotes across numeric widths at evaluation anyway.
	t.Run("a UUID operand with a non-constant STRING item", func(t *testing.T) {
		udb := openUUIDInDB(t)
		mwjoMustExec(t, udb, ctx, "INSERT INTO u (id, uu, us) VALUES "+
			// row 1: us holds uu's own text  -> `uu IN (us)` matches
			"(1, '11111111-1111-1111-1111-111111111111', '11111111-1111-1111-1111-111111111111'), "+
			// row 2: us holds a DIFFERENT uuid's text -> no match
			"(2, '22222222-2222-2222-2222-222222222222', '33333333-3333-3333-3333-333333333333')")

		got, err := mmRows(t, ctx, udb, "SELECT id FROM u WHERE uu IN (us) ORDER BY id")
		if err != nil {
			t.Fatalf("a UUID IN with a column item failed to plan: %v", err)
		}
		if !mmEqRows(got, []string{"1"}) {
			t.Fatalf("a UUID compared against a runtime STRING item did not match\n"+
				"  got  %v\n  want [1]\n"+
				"  (an empty answer means the [16]byte field was compared against a raw string "+
				"and cmpAny declined the pair — equal values silently treated as non-matches)", got)
		}

		// The negation, where the same failure reads as ordinary rather than as
		// an obviously empty result.
		gotNot, err := mmRows(t, ctx, udb, "SELECT id FROM u WHERE uu NOT IN (us) ORDER BY id")
		if err != nil {
			t.Fatalf("a UUID NOT IN with a column item failed to plan: %v", err)
		}
		if !mmEqRows(gotNot, []string{"2"}) {
			t.Fatalf("a UUID NOT IN over a runtime STRING item is wrong\n"+
				"  got  %v\n  want [2]\n"+
				"  (returning row 1 as well is the un-promoted comparison admitting a row whose "+
				"value DOES appear in the list — the silent direction)", gotNot)
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
