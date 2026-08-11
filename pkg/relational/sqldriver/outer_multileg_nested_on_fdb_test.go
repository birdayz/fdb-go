package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// A NESTED path in the ON clause of an OUTER third leg — `m.n.sk = r.n.sk` over
// `l JOIN m ... LEFT JOIN r ...` — used to fail loud with "no frontier row
// resolved" (the multi-leg positional row's correct-or-loud guard).
//
// The gated join's predicate bake identifies a leg reference by legRef, which
// excluded every MULTI-ACCESSOR path on the reading that only the bake's own
// machinery mints one. That stopped being true when a user-written nested
// descent started arriving as ONE unpinned FieldValue with the descent in its
// remaining accessors: the reference was declined, kept its leg correlation,
// nothing bound that correlation at execution, and the read fell through to the
// multi-leg row. Machinery-ownership is the FRONTIER PIN, not the arity.
//
// THE ARMS ARE A GRID, NOT A REPRODUCER, and it asserts VALUES rather than the
// absence of an error, because the two ways this can be wrong are opposite:
//   - admitting the reference without fusing its suffix bakes the address of the
//     enclosing STRUCT and drops the descent. That address is a real leg column,
//     so nothing rejects it — the predicate compares a BIGINT against a whole
//     struct and quietly matches nothing. WRONG ROWS, not an error.
//   - declining it again restores the loud failure.
//
// A–E are the arms that already worked and they pin the no-regression half:
// two-way (nested, inner and outer), three-way inner, three-way outer FLAT, and
// three-way outer nested on the PRESERVED side only. F–H are the three that
// failed — the defect needs three legs, an OUTER third leg, AND a nested path
// reading the multi-leg side.
//
// THE CLASS BOUNDARY IS MEASURED, NOT ARGUED. I–L extend the grid along the
// three axes a "structurally covered" argument would have been made about, and
// every one of them FAILED under the pre-fix code with the same loud error — so
// they are in class, not decoration:
//
//   - I: MIXED flat + nested across the two sides. It is also the only arm whose
//     PLAN moves, and it is asserted (see planHas/planLacks): the conjunct goes
//     from lazy/pushdown-eligible to join-resident, which turns a residual
//     predicate over a full inner scan into a keyed probe. D is its control —
//     same join shape, flat comparand, plan measured identical on both sides of
//     the mutation.
//   - J: DEPTH 3. FuseNestedSuffix loops and legRef is arity-blind, so no new
//     mechanism is involved; an arity cap re-introduced at any value >= 3 would
//     pass every depth-2 arm and fail only this one.
//   - K, L: RIGHT and FULL. The defect was never LEFT-specific — the bake does
//     not see the null-supply direction — and these two say so by measurement.
//   - M: FOUR legs. This arm was going to be written up as out of class on the
//     reasoning that a wider cluster takes a different admission path. Driving
//     it instead showed it failing with the same error on correlation P, so the
//     reasoning was wrong and the arm is a pin rather than a footnote.
//
// OUT OF CLASS, stated so the boundary is not mistaken for exhaustiveness: the
// EXISTS-over-gathered-cluster family. Its own leg walk (rebaseLegRefsToBox)
// declines a nested descent DELIBERATELY — it bakes a one-step address and
// carries no suffix, so admitting one would drop the descent — and that
// narrowness is pinned at its own site by
// TestRebaseLegRefsToBox_DeclinesANestedDescent, which also names what re-arms
// if it is widened.
func TestFDB_OuterMultilegNestedOnPredicate(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_omlnon")
	mustExec(t, setup, ctx, "CREATE DATABASE /testdb_omlnon")
	// `d` makes a DEPTH-3 descent (`m.n.d.dk`) expressible. Depth is not a
	// separate mechanism — FuseNestedSuffix loops over the whole suffix and
	// legRef is arity-blind — but "structurally covered" is not a measurement,
	// and an arity cap re-introduced at any value >= 3 would pass every depth-2
	// arm below and fail only this one.
	mustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE omlnon_tmpl "+
		"CREATE TYPE AS STRUCT dst (dk BIGINT) "+
		"CREATE TYPE AS STRUCT gst (sk BIGINT, co BIGINT, d dst) "+
		"CREATE TABLE nt(id BIGINT, sk BIGINT, n gst, PRIMARY KEY(id))")
	mustExec(t, setup, ctx, "CREATE SCHEMA /testdb_omlnon/s WITH TEMPLATE omlnon_tmpl")

	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///testdb_omlnon?cluster_file=%s&schema=s", clusterFilePath))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// n.sk DELIBERATELY collides across rows 1 and 2 while the flat sk does not:
	// the nested key must produce a DIFFERENT multiplicity from the flat one, so
	// a descent that silently reads the struct root (or the wrong column) cannot
	// coincide with the right answer.
	// n.d.dk repeats the same 1,1,2 pattern one level deeper, so a depth-3
	// descent that stopped short at `n.d` (a struct) would match nothing and be
	// visible as [1 2 3] rather than [1 1 2 2 3].
	mustExec(t, db, ctx, "INSERT INTO nt VALUES "+
		"(1, 10, (1, 7, (1))), (2, 20, (1, 8, (1))), (3, 30, (2, 9, (2)))")

	run := func(q string) ([]string, error) {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var v sql.NullInt64
			if err := rows.Scan(&v); err != nil {
				return nil, err
			}
			if v.Valid {
				out = append(out, fmt.Sprint(v.Int64))
			} else {
				out = append(out, "NULL")
			}
		}
		return out, rows.Err()
	}

	// planHas / planLacks are EXPLAIN substring assertions, set only where the
	// plan SHAPE is part of the claim. Rows alone cannot see the move that
	// matters for arm I, and the standing plan-shape golden cannot either — the
	// golden pins the corpus, and the corpus has no shape of this family.
	for _, tc := range []struct {
		name, query string
		want        []string
		planHas     []string
		planLacks   []string
	}{
		// ---- A–E: already correct before the fix; they pin that it moved nothing.
		{
			"A_two-way-inner-nested",
			"SELECT l.id FROM nt AS l JOIN nt AS r ON l.n.sk = r.n.sk ORDER BY l.id",
			[]string{"1", "1", "2", "2", "3"},
			nil,
			nil,
		},
		{
			"B_two-way-outer-nested",
			"SELECT l.id FROM nt AS l LEFT JOIN nt AS r ON l.n.sk = r.n.sk ORDER BY l.id",
			[]string{"1", "1", "2", "2", "3"},
			nil,
			nil,
		},
		{
			"C_three-way-inner-nested",
			"SELECT l.id FROM nt AS l JOIN nt AS m ON l.id = m.id JOIN nt AS r ON m.n.sk = r.n.sk ORDER BY l.id",
			[]string{"1", "1", "2", "2", "3"},
			nil,
			nil,
		},
		{
			// The flat twin of F. Same three legs, same outer third leg, ONE
			// accessor — this is the arm the old arity gate admitted, and its
			// row set differs from F's, which is what makes F's assertion a
			// statement about the descent rather than about the join shape.
			"D_three-way-outer-flat",
			"SELECT l.id FROM nt AS l JOIN nt AS m ON l.id = m.id LEFT JOIN nt AS r ON m.sk = r.sk ORDER BY l.id",
			[]string{"1", "2", "3"},
			// The PLAN CONTROL for arm I. `m.sk` is not a comparand any scan of
			// NT can be keyed on, so this shape keeps its residual-predicate
			// nested loop with the fix as without it. Measured identical on both
			// sides of the mutation — which is what makes arm I's move
			// attributable to the nested reference rather than to the fix
			// re-planning every three-leg outer join.
			[]string{"NestedLoopJoin(LEFT OUTER"},
			nil,
		},
		{
			// Nested on the NULL-supplied side only: no reference reads the
			// multi-leg side, so the bake was never asked.
			"E_three-way-outer-nested-preserved-only",
			"SELECT l.id FROM nt AS l JOIN nt AS m ON l.id = m.id LEFT JOIN nt AS r ON m.sk = r.n.sk ORDER BY l.id",
			[]string{"1", "2", "3"},
			nil,
			nil,
		},

		// ---- F–H: the three arms that failed loud. m.n.sk is 1,1,2 over
		// m.id 1,2,3, so a correct descent multiplies rows 1 and 2 by two.
		{
			"F_three-way-outer-nested-both",
			"SELECT l.id FROM nt AS l JOIN nt AS m ON l.id = m.id LEFT JOIN nt AS r ON m.n.sk = r.n.sk ORDER BY l.id",
			[]string{"1", "1", "2", "2", "3"},
			nil,
			nil,
		},
		{
			// Nested on the MULTI-LEG side only, against a flat r.sk (10/20/30)
			// no n.sk can equal — every row is NULL-supplied exactly once. A
			// dropped descent would compare the struct against r.sk, which also
			// matches nothing, so this arm alone cannot separate the two; it is
			// here because it is a distinct failing shape, and F/H carry the
			// discrimination.
			"G_three-way-outer-nested-multileg-only",
			"SELECT l.id FROM nt AS l JOIN nt AS m ON l.id = m.id LEFT JOIN nt AS r ON m.n.sk = r.sk ORDER BY l.id",
			[]string{"1", "2", "3"},
			nil,
			nil,
		},
		{
			"H_three-way-outer-nested-first-leg",
			"SELECT l.id FROM nt AS l JOIN nt AS m ON l.id = m.id LEFT JOIN nt AS r ON l.n.sk = r.n.sk ORDER BY l.id",
			[]string{"1", "1", "2", "2", "3"},
			nil,
			nil,
		},

		// ---- The CLASS, not just the case.
		{
			// MIXED flat + nested across the two sides. This is the arm whose
			// PLAN moves: with the nested side invisible to legRef the conjunct
			// counted ONE leg alias, failed the cross-leg test and stayed LAZY
			// (pushdown-eligible); now it counts two and bakes into the join.
			// `m.n.sk` is 1,1,2 and `r.id` is 1,2,3, so m=1 and m=2 both match
			// r=1 while m=3 matches r=2 — a multiplicity no flat key here
			// reproduces.
			"I_three-way-outer-mixed-nested-and-flat",
			"SELECT l.id FROM nt AS l JOIN nt AS m ON l.id = m.id LEFT JOIN nt AS r ON m.n.sk = r.id ORDER BY l.id",
			[]string{"1", "2", "3"},
			// MEASURED, both sides of the mutation. Declined:
			//   NestedLoopJoin(LEFT OUTER, [1 preds], FlatMap(…), Scan(NT))
			// admitted:
			//   FlatMap(…, inner=DefaultOnEmpty(Scan(NT, [=])))
			// The baked conjunct is a comparand the inner scan can be keyed on,
			// so the residual predicate over a full inner scan becomes an
			// equality probe. This is the one place the fix moves a plan, and it
			// moves it strictly downward in cost.
			[]string{"DefaultOnEmpty(Scan(NT, [=]))"},
			[]string{"NestedLoopJoin"},
		},
		{
			// DEPTH 3. legRef is arity-blind and FuseNestedSuffix loops, so this
			// needs no new mechanism — which is exactly why it needs a
			// measurement rather than an argument.
			"J_three-way-outer-nested-depth-three",
			"SELECT l.id FROM nt AS l JOIN nt AS m ON l.id = m.id LEFT JOIN nt AS r ON m.n.d.dk = r.n.d.dk ORDER BY l.id",
			[]string{"1", "1", "2", "2", "3"},
			nil,
			nil,
		},
		{
			// RIGHT and FULL are the other two outer flavours. The defect was
			// never LEFT-specific — it needed an OUTER third leg, and the null
			// supply direction is not what the bake sees — but "not specific to
			// LEFT" is a claim, so both are driven. RIGHT preserves r, so every
			// r row survives and the l side is null-supplied where it does not
			// match: r.n.sk 1,1,2 against m.n.sk 1,1,2 gives 2+2+1 matches.
			"K_three-way-right-outer-nested",
			"SELECT l.id FROM nt AS l JOIN nt AS m ON l.id = m.id RIGHT JOIN nt AS r ON m.n.sk = r.n.sk ORDER BY l.id",
			[]string{"1", "1", "2", "2", "3"},
			nil,
			nil,
		},
		{
			// FULL: every row matches on both sides here, so no padding is
			// added and the row set equals the inner one. The arm is about the
			// join FLAVOUR reaching the bake, not about null supply.
			"L_three-way-full-outer-nested",
			"SELECT l.id FROM nt AS l JOIN nt AS m ON l.id = m.id FULL JOIN nt AS r ON m.n.sk = r.n.sk ORDER BY l.id",
			[]string{"1", "1", "2", "2", "3"},
			nil,
			nil,
		},
		{
			// FOUR legs. Not a deeper version of the three-leg case — a wider
			// cluster takes a different admission path — so it is driven rather
			// than reasoned about. m/p join l on id; the outer leg's ON reads
			// p.n.sk (1,1,2), matching r.n.sk with multiplicity 2,2,1.
			"M_four-way-outer-nested",
			"SELECT l.id FROM nt AS l JOIN nt AS m ON l.id = m.id JOIN nt AS p ON m.id = p.id " +
				"LEFT JOIN nt AS r ON p.n.sk = r.n.sk ORDER BY l.id",
			[]string{"1", "1", "2", "2", "3"},
			nil,
			nil,
		},
	} {
		if len(tc.planHas) > 0 || len(tc.planLacks) > 0 {
			var plan string
			if err := db.QueryRowContext(ctx, "EXPLAIN "+tc.query).Scan(&plan); err != nil {
				t.Errorf("%s: EXPLAIN %s: %v", tc.name, tc.query, err)
			} else {
				for _, want := range tc.planHas {
					if !strings.Contains(plan, want) {
						t.Errorf("%s: plan does not contain %q\n\tplan: %s", tc.name, want, plan)
					}
				}
				for _, absent := range tc.planLacks {
					if strings.Contains(plan, absent) {
						t.Errorf("%s: plan still contains %q — the conjunct did not bake into "+
							"the join, so it stayed a residual predicate over a full inner scan"+
							"\n\tplan: %s", tc.name, absent, plan)
					}
				}
			}
		}
		got, err := run(tc.query)
		if err != nil {
			t.Errorf("%s: %s\n\tunexpected error: %v\n\twant rows %v", tc.name, tc.query, err, tc.want)
			continue
		}
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Errorf("%s: %s\n\t got %v\n\twant %v\n"+
				"A row set that is neither the error nor the expected rows is the SILENT half of "+
				"this defect: the nested descent was dropped and the predicate compared against "+
				"the enclosing struct.", tc.name, tc.query, got, tc.want)
		}
	}
}
