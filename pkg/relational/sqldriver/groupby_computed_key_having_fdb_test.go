package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

// TestFDB_ComputedGroupKeyRereadBindsItsOwnSlot pins that a COMPUTED grouping
// key (`GROUP BY c1 + 1`) re-read in a post-aggregate clause binds to the
// aggregate output slot the key occupies — and not to whatever slot the
// reference's PRE-aggregate ordinal happens to land on.
//
// IT WAS SILENT WRONG ROWS, and this file began life as the characterization of
// that. rebasePostAggregateGroupKeyValue walked FieldValues and asked, per
// field, which grouping key that field IS — through a
// `gk.Value.(*values.FieldValue)` assertion. A computed key's Value is an
// ArithmeticValue, so the assertion failed for every key, no reference ever
// matched, and the reference survived as an INPUT-relative read evaluated
// against the aggregate's OUTPUT row:
//
//	SELECT max(c2) FROM flat GROUP BY c1 + 1 HAVING c1 + 1 > 200
//	  keys are 101 and 141, so the correct answer is ZERO rows -> returned TWO.
//
//	SELECT c1 + 1, max(c2) FROM flat GROUP BY c1 + 1 HAVING c1 + 1 > 200 AND max(c2) > 0
//	  returned [101 203] [141 330] — the key column saying 101 and 141 while a
//	  `> 200` HAVING admitted both. One result set contradicting itself.
//
// WHICH SLOT IT READ, identified rather than merely observed. `c1` carries input
// ordinal 1 over [ID C1 C2]; the output row is [(C1 + 1), MAX(C2)], whose slot 1
// is the AGGREGATE. That predicts admitting BOTH groups on `> 200` and NEITHER
// on `< 200` — both measured before the fix, in opposite directions, and both
// measured to flip with it. A one-directional test cannot tell a wrong slot from
// a dropped predicate, which is why both directions are asserted below.
//
// THE FIX IS JAVA'S, and the unit of matching is the point: Expression.pullUp
// (fdb-relational-core …/query/Expression.java at 4.12.11.0) walks the
// post-aggregate expression with `underlying.replace(subExpression -> …)` and
// asks, for EVERY SUB-EXPRESSION, whether the group-by result value produces it,
// replacing the whole matched sub-expression. Its SELECT-list twin
// Expressions.pullUp is the same walk. Neither requires the grouping item to be
// a field. rebasePostAggregateComputedGroupKey is that walk.
//
// The BARE-key control is what makes this a computed-key finding rather than
// "HAVING over a grouped query is broken", and the NESTED arm is the same defect
// reported loudly — it died as `not resolvable in the runtime row` only because
// the fused root's ordinal fell outside a 2-wide output row, which is luck and
// not a guard. Both are asserted so a fix that satisfies only one half cannot
// pass.
func TestFDB_ComputedGroupKeyRereadBindsItsOwnSlot(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/cgkh")
	mustExec(t, setup, ctx, "CREATE DATABASE /cgkh")
	mustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE cgkh_tmpl "+
		"CREATE TYPE AS STRUCT st1(y BIGINT, z BIGINT) "+
		"CREATE TYPE AS STRUCT st2(w BIGINT, x BIGINT) "+
		"CREATE TYPE AS STRUCT st3(u st2, v st1) "+
		"CREATE TYPE AS STRUCT st4(s BIGINT, t BIGINT) "+
		"CREATE TABLE nested(id BIGINT, q st4, r st3, PRIMARY KEY(q.s, r.u.w)) "+
		"CREATE TABLE flat(id BIGINT, c1 BIGINT, c2 BIGINT, PRIMARY KEY(id))")
	mustExec(t, setup, ctx, "CREATE SCHEMA /cgkh/s WITH TEMPLATE cgkh_tmpl")

	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///cgkh?cluster_file=%s&schema=s", clusterFilePath))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// The keys (101, 141) and the aggregates (203, 330) are deliberately far
	// apart and on OPPOSITE sides of the comparand 200: reading the aggregate
	// where the key was named flips every predicate below, so a wrong slot
	// changes the ROW COUNT rather than coinciding with the right answer.
	mustExec(t, db, ctx, "INSERT INTO flat VALUES "+
		"(1, 100, 200), (2, 100, 201), (3, 100, 202), (4, 100, 203), "+
		"(5, 140, 330), (6, 140, 329), (7, 140, 328), (8, 140, 327)")
	mustExec(t, db, ctx, "INSERT INTO nested VALUES "+
		"(1, (200, 1), ((5, 15), (10, 100))), "+
		"(2, (201, 2), ((5, 15), (10, 100))), "+
		"(3, (202, 3), ((5, 15), (10, 100))), "+
		"(4, (203, 4), ((5, 15), (10, 100))), "+
		"(5, (330, 5), ((5, 15), (10, 140))), "+
		"(6, (329, 6), ((5, 15), (10, 140))), "+
		"(7, (328, 7), ((5, 15), (10, 140))), "+
		"(8, (327, 8), ((5, 15), (10, 140)))")

	rowsOf := func(t *testing.T, q string, cols int) [][]int64 {
		t.Helper()
		rs, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rs.Close()
		var out [][]int64
		for rs.Next() {
			row := make([]int64, cols)
			ptrs := make([]any, cols)
			for i := range row {
				ptrs[i] = &row[i]
			}
			if err := rs.Scan(ptrs...); err != nil {
				t.Fatalf("scan %q: %v", q, err)
			}
			out = append(out, row)
		}
		if err := rs.Err(); err != nil {
			t.Fatalf("rows %q: %v", q, err)
		}
		return out
	}
	eq := func(t *testing.T, q string, cols int, want, why string) {
		t.Helper()
		got := rowsOf(t, q, cols)
		if fmt.Sprint(got) != want {
			t.Fatalf("%s\n  rows = %v\n  want = %s\n  %s", q, got, want, why)
		}
	}

	const slotHint = "Reading the AGGREGATE slot where the KEY was named is the " +
		"defect this pins: `c1` carries input ordinal 1 over [ID C1 C2] and the " +
		"output row is [(C1 + 1), MAX(C2)], whose slot 1 is MAX(C2)."

	t.Run("flat_excludes_every_group_when_no_key_qualifies", func(t *testing.T) {
		// BOTH keys (101, 141) are below 200, so nothing qualifies. Reading the
		// aggregate instead gives 204 and 331 and admits both.
		eq(t, "SELECT max(c2) FROM flat GROUP BY c1 + 1 HAVING c1 + 1 > 200", 1, "[]", slotHint)
	})

	t.Run("flat_admits_every_group_when_every_key_qualifies", func(t *testing.T) {
		// THE OPPOSITE DIRECTION, and it is required rather than symmetric
		// padding: an arm that only ever over-admits is equally consistent with
		// the predicate being DROPPED. Under-admitting on the mirrored
		// comparison is what says the predicate is evaluated, against a slot.
		eq(t, "SELECT max(c2) FROM flat GROUP BY c1 + 1 HAVING c1 + 1 < 200 AND max(c2) > 0",
			1, "[[203] [330]]", slotHint)
	})

	t.Run("flat_the_key_column_and_the_having_agree", func(t *testing.T) {
		// The sharpest form: the projected key and the HAVING must describe the
		// same rows. Before the fix this returned [[101 203] [141 330]] for the
		// `> 200` spelling — a result set no reading of the data makes correct.
		eq(t, "SELECT c1 + 1, max(c2) FROM flat GROUP BY c1 + 1 HAVING c1 + 1 > 200 AND max(c2) > 0",
			2, "[]", slotHint)
		eq(t, "SELECT c1 + 1, max(c2) FROM flat GROUP BY c1 + 1 HAVING c1 + 1 > 120 AND max(c2) > 0",
			2, "[[141 330]]", slotHint)
	})

	t.Run("flat_not_and_order_by_over_the_computed_key", func(t *testing.T) {
		// NOT is non-pushable for the same reason AND is, and ORDER BY is the
		// other post-aggregate consumer fed by the same rebase. DESC so a key
		// that lost its binding shows as the ascending sequence.
		eq(t, "SELECT c1 + 1, max(c2) FROM flat GROUP BY c1 + 1 HAVING NOT (c1 + 1 > 120)",
			2, "[[101 203]]", slotHint)
		eq(t, "SELECT max(c2) FROM flat GROUP BY c1 + 1 ORDER BY c1 + 1 DESC",
			1, "[[330] [203]]", slotHint)
	})

	t.Run("flat_a_non_arithmetic_computed_key", func(t *testing.T) {
		// COALESCE, so the fix is not read as arithmetic-specific: the unit of
		// matching is the sub-expression, whatever kind it is.
		eq(t, "SELECT COALESCE(c1, 0), max(c2) FROM flat GROUP BY COALESCE(c1, 0) "+
			"HAVING COALESCE(c1, 0) > 120 AND max(c2) > 0", 2, "[[140 330]]", slotHint)
	})

	t.Run("a_bare_key_is_the_control_and_was_always_correct", func(t *testing.T) {
		// Without this the arms above could be read as "HAVING over a grouped
		// query is broken", which is not the finding. A BARE key's gk.Value IS a
		// FieldValue, so the pre-existing FieldValue walk already pinned it.
		eq(t, "SELECT max(c2) FROM flat GROUP BY c1 HAVING c1 > 200 AND max(c2) > 0",
			1, "[]", "the BARE-key control regressed, so the fix is wider than the computed-key arm")
		eq(t, "SELECT max(c2) FROM flat GROUP BY c1 HAVING c1 < 200 AND max(c2) > 0",
			1, "[[203] [330]]", "the BARE-key control regressed")
	})

	t.Run("the_nested_twin_answers_instead_of_dying", func(t *testing.T) {
		// The same defect, whose nested spelling died as `ordinal resolution:
		// field "R" not resolvable in the runtime row (ordinal 2, row columns
		// [(R.V.Z + 1) MAX(Q.S)])` — loud only because the fused root's ordinal
		// fell outside a 2-wide row. Same fix, and it must move with the flat
		// half: a change that satisfies only the loud half leaves the silent
		// half shipping.
		eq(t, "SELECT max(q.s) FROM nested GROUP BY r.v.z + 1 HAVING r.v.z + 1 > 121",
			1, "[[330]]", slotHint)
		eq(t, "SELECT max(q.s) FROM nested GROUP BY r.v.z + 1 HAVING r.v.z + 1 > 121 AND max(q.s) > 250",
			1, "[[330]]", slotHint)
		eq(t, "SELECT r.v.z + 1, max(q.s) FROM nested GROUP BY r.v.z + 1 HAVING r.v.z + 1 < 121",
			2, "[[101 203]]", slotHint)
		eq(t, "SELECT max(q.s) FROM nested GROUP BY COALESCE(r.v.z, 0) "+
			"HAVING COALESCE(r.v.z, 0) > 120 AND max(q.s) > 250", 1, "[[330]]", slotHint)
	})
}
