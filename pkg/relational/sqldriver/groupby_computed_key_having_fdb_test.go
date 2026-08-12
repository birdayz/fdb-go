package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
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
//
// TWO ARMS TEST THE BINDING RATHER THAN THE WALK, and they are here because
// every other arm is correct by coincidence in the same way the defect was:
//
//   - two_computed_keys… — it is the DELIBERATE witness for the computed walk's
//     slot index. Most arms group by one key, so the recorded slot is always 0
//     and cannot be told from a hardcoded zero, while that index IS the binding.
//     Of the arms that do use two keys, the duplicate ones are refused 42702
//     before the walk runs and the join one is refused at output construction;
//     the parenthesised one, however, DOES reach this walk — both its keys
//     are non-FieldValue, so it binds at slot 1.
//     MEASURED at 14 arms: pinning the computed walk's mint to ordinal 0 reddens
//     TWO arms — this one and a_parenthesised_computed_key_twin… — leaving 12
//     green. Both reddening is itself the proof that the paren arm is on the
//     computed route, since that mutation touches no other walk. This arm stays
//     the deliberate witness because the paren arm also passes for an unrelated
//     reason (see its note on m1), so it cannot be relied on for the index.
//     The sibling walk has its own such arm (a_nested_key_in_a_nonzero_slot),
//     because the two walks record the slot at separate sites. Do NOT read that
//     as a clean partition: that arm STRADDLES both routes. It groups by
//     `q.t + 1, r.v.z`, so it drives the computed walk as well, and it is RED
//     under a revert of that walk. It is green under the pin-to-zero mutation
//     for a narrower reason — its computed key sits at slot 0, where pinning to
//     0 is a NO-OP — not because the mutation cannot reach it. If that key is
//     ever moved off slot 0, the arm starts reddening here and nothing about the
//     code will look wrong, so the greenness is a fact about the fixture and is
//     recorded as one;
//   - a_duplicate_computed_key… — a NEGATIVE result. These spellings are refused
//     42702 before the rebase is reached at all. The arm
//     pins that refusal and names what re-arms it. It does NOT claim the refusal
//     is guaranteed: the "both sites use the same predicate" argument that once
//     stood here is false, and the arm below
//     (a_parenthesised_computed_key_twin…) is the counterexample.
//
// THREE FURTHER ARMS COVER THE NESTED ROUTE, which is a DIFFERENT walk. RFC-230
// retired the refusal that made a nested path unusable as a grouping key, so
// `GROUP BY r.v.z HAVING r.v.z > 120` became constructible at the same moment
// this file's fix changed how a re-read key binds; the two shapes had never run
// together. A nested key resolves to a fused multi-accessor FieldValue, so it is
// bound by the SIBLING FieldValue walk rather than by the computed walk the arms
// above drive — a route those arms cannot speak for. All three were measured
// CORRECT on first run; they are pins, not fixes. The duplicate arm is the
// sharpest of them: on the nested route the duplicate gate and the rebase matcher
// decide identity by DIFFERENT predicates (a name-based compare vs semantic
// equality), so that interlock is measured rather than structural.
//
// SO IS THE COMPUTED ONE, and saying otherwise was this file's own mistake for
// two revisions. The computed gate arm is nominally semantic, but it runs only
// when both keys resolve through a walk WEAKER than the one that mints them, and
// it returns false on `(c1 + 1)` vs `c1 + 1` even when it does run. NEITHER route
// has a structural guarantee; they differ only in how easy the counterexample is
// to write.
//
// TWO ARMS ONCE PINNED DIVERGENCES THIS FILE'S FIX DID NOT CAUSE; ONE OF THEM IS
// NOW CLOSED AND THE OTHER IS NOT, and the difference is worth stating because
// they used to be described as sharing one fix.
//
// Under a join the gate's qualifier normalization is switched off, so
// `… JOIN … GROUP BY a.r.v.z, r.v.z` puts two semantically equal keys in front of
// the aggregate; on a plain table `GROUP BY (c1 + 1), c1 + 1` slips the computed
// gate on a Value-type mismatch. Go answered where Java stops in both.
//
//   - THE JOIN ONE IS CLOSED. `groupByOutputConstructionPullUp` refuses two
//     semantically equal grouping keys at output construction, which is where
//     Java refuses them (LogicalOperator.java:454 through the asserting
//     Expressions.pullUp) — before any SELECT-list or HAVING pull-up and without
//     needing a post-aggregate reference to exist. All spellings now raise
//     42702, including two that project no key and carry no HAVING.
//   - THE PARENTHESISED ONE IS NOT, and no guard can close it: `(c1 + 1)` is a
//     RecordConstructorValue and `c1 + 1` an ArithmeticValue, so no semantic
//     matcher equates them at construction either. It needs the normalization
//     step. Its arm still asserts VALUES.
//
// The join case was PRE-EXISTING rather than introduced here (the flat twin
// diverges identically, and the original branch's delta over post-#719 master
// deleted no source line). Both remain booked in TODO.md, now with their
// closable sets listed apart so neither is credited with the other's work.
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

	t.Run("two_computed_keys_bind_their_own_slot_and_not_slot_zero", func(t *testing.T) {
		// THE SLOT INDEX IS THE BINDING, and a single-key arm cannot tell the
		// recorded loop index from a hardcoded zero. This arm exists for the
		// index alone, and it is the DELIBERATE witness for it on the computed
		// walk — MEASURED at 14 arms, pinning that walk's mint to ordinal 0
		// reddens TWO arms: this one and a_parenthesised_computed_key_twin…,
		// leaving 12 green.
		//
		// It is not the only witness, though, and the difference matters when
		// reading a mutation result. a_parenthesised_computed_key_twin also puts
		// a second key in front of THIS walk: its HAVING reference resolves to an
		// ArithmeticValue matched against key index 1 (the instrumentation in
		// that arm reports the keys as [RecordConstructorValue, ArithmeticValue],
		// which is the computed loop's own view). So it is a second, incidental
		// witness — but it cannot replace this one, because it also passes for a
		// reason unrelated to the index (see its own note on m1).
		//
		// The two duplicate arms genuinely do not substitute: both are refused
		// 42702 before any walk runs.
		//
		// The groups are the eight (c1+1, c2+1) pairs: (101,201..204) and
		// (141,328..331). The predicates are chosen so that reading the OTHER
		// key's slot changes the answer in every arm, which is what the obvious
		// fixture does NOT do — `c2 + 1 > 300` and `c1 + 1 > 120` happen to
		// select the same four rows, so an arm built on either would pass while
		// binding the wrong slot.
		//
		//   c2 + 1 > 203  ->  5 rows; on slot 0 (101/141) it is 0.
		//   c2 + 1 < 203  ->  2 rows; on slot 0 it is 8.
		//   c1 + 1 > 120  ->  4 rows; on slot 1 (201..331) it is 8.
		//   c1 + 1 < 120  ->  4 rows; on slot 1 it is 0.
		//
		// The first is the sharpest: its answer CROSSES the slot-0 grouping
		// boundary — one 101 row and four 141 rows — so no predicate evaluated
		// against slot 0 can produce it at all.
		const slotIdx = "This arm pins the SLOT INDEX. Reading the other grouping " +
			"key's slot changes this answer, which is what separates the recorded " +
			"loop index from a hardcoded 0."
		eq(t, "SELECT c1 + 1, c2 + 1, max(id) FROM flat GROUP BY c1 + 1, c2 + 1 HAVING c2 + 1 > 203",
			3, "[[101 204 4] [141 328 8] [141 329 7] [141 330 6] [141 331 5]]", slotIdx)
		eq(t, "SELECT c1 + 1, c2 + 1, max(id) FROM flat GROUP BY c1 + 1, c2 + 1 HAVING c2 + 1 < 203",
			3, "[[101 201 1] [101 202 2]]", slotIdx)
		// The SAME query shape with the predicate on the FIRST key, so slot 0 is
		// exercised deliberately rather than by being every arm's only option.
		eq(t, "SELECT c1 + 1, c2 + 1, max(id) FROM flat GROUP BY c1 + 1, c2 + 1 HAVING c1 + 1 > 120",
			3, "[[141 328 8] [141 329 7] [141 330 6] [141 331 5]]", slotIdx)
		eq(t, "SELECT c1 + 1, c2 + 1, max(id) FROM flat GROUP BY c1 + 1, c2 + 1 HAVING c1 + 1 < 120",
			3, "[[101 201 1] [101 202 2] [101 203 3] [101 204 4]]", slotIdx)
		// With an aggregate in the predicate, so the arm also covers the shape
		// where pushdown is refused for a second, independent reason.
		eq(t, "SELECT c1 + 1, c2 + 1, max(id) FROM flat GROUP BY c1 + 1, c2 + 1 "+
			"HAVING c2 + 1 > 203 AND max(id) > 0",
			3, "[[101 204 4] [141 328 8] [141 329 7] [141 330 6] [141 331 5]]", slotIdx)
	})

	t.Run("a_duplicate_computed_key_is_refused_before_the_rebase", func(t *testing.T) {
		// A NEGATIVE RESULT, and it is what says the rebase's first-match loop
		// cannot silently pick a slot where Java raises.
		//
		// Java's HAVING pull-up ends in Iterables.getOnlyElement, which THROWS
		// on a multi-match, and its SELECT twin asserts
		// `pulledUpExpressionMap.get(subExpression).size() == 1` with
		// AMBIGUOUS_COLUMN (Expressions.java:112). Go's rebase instead takes the
		// FIRST matching key by loop index. That difference is only safe while no
		// query can present two keys that both match — and none can:
		// `GROUP BY c1 + 1, c1 + 1` is refused 42702 upstream, carrying Java's
		// own wording verbatim.
		//
		// THE REFUSAL IS MEASURED, NOT STRUCTURAL, and an earlier version of this
		// comment claimed otherwise. The claim was: the gate decides key identity
		// with values.SemanticEqualsUnderAliasMap, the SAME predicate the rebase
		// matches with, so two keys that could both match are semantically equal
		// and already refused. That is an impossibility proof, and it is invalid.
		//
		// The gate is a three-arm switch and its semantic arm needs BOTH keys to
		// resolve through Resolver.WalkExpression, which is weaker than the
		// WalkExpressionForProjection that mints the key Value — it will not fold
		// a comparison. And even when the semantic arm DOES run it can return
		// false for one expression written two ways: `(c1 + 1)` resolves to a
		// RecordConstructorValue wrapping what `c1 + 1` resolves to bare. Both
		// holes are measured and pinned by
		// a_parenthesised_computed_key_twin_is_NOT_refused_either.
		//
		// What survives is the OBSERVATION, which is what this arm asserts: these
		// spellings are refused 42702 before the rebase. Instrumenting the
		// first-match loop shows it never sees a multi-match — but that is the
		// wrapper mismatch keeping the keys distinguishable, not the gate.
		//
		// WHAT RE-ARMS THE HAZARD, named because that is the point of pinning a
		// negative: the two predicates diverging. If the duplicate gate is ever
		// narrowed — or the rebase's matcher widened, e.g. to match under a
		// non-identity alias map — a multi-match becomes constructible and Go
		// would silently bind the first key where Java raises. Then this arm
		// must become the assertion that Go raises too.
		for _, q := range []string{
			"SELECT max(id) FROM flat GROUP BY c1 + 1, c1 + 1 HAVING c1 + 1 > 120",
			"SELECT c1 + 1, max(id) FROM flat GROUP BY c1 + 1, c1 + 1",
			"SELECT max(id) FROM flat GROUP BY c1 + 1, c1 + 1",
		} {
			_, err := db.QueryContext(ctx, q)
			if err == nil {
				t.Fatalf("%s now PLANS.\n"+
					"  Two semantically equal computed keys reaching the rebase makes its "+
					"first-match loop able to choose a slot silently, where Java raises "+
					"AMBIGUOUS_COLUMN. Either restore the refusal or make the rebase "+
					"raise on a multi-match, and re-point this arm at whichever it is.", q)
			}
			if !strings.Contains(err.Error(), "42702") {
				t.Fatalf("%s: got %v, want 42702.\n"+
					"  The CODE is the point: this must be the duplicate-grouping-key "+
					"refusal (Java's AMBIGUOUS_COLUMN), not some unrelated rejection that "+
					"happens to keep the shape away from the rebase.", q, err)
			}
		}
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

	// THE THREE ARMS BELOW PIN A COMPOSITION NEITHER CHANGE COULD TEST. A NESTED
	// path is only usable as a grouping key since RFC-230 retired the
	// `0AF00 grouping by the nested field %q is not supported` refusal and
	// replaced it with the struct-descent arm in upgradeAggregateOperands. So a
	// nested key re-read in HAVING became constructible at the same moment this
	// file's fix changed how a re-read key binds — and the two met without ever
	// having been run together.
	//
	// ALL THREE WERE MEASURED CORRECT ON FIRST RUN; nothing here fixes anything.
	// They are pinned because "correct today, untested" is the state a later
	// change breaks silently, and because a nested key reaches the binder by a
	// DIFFERENT route than the computed key this file was written for: it is a
	// FieldValue, so it is the SIBLING walk's business, not
	// rebasePostAggregateComputedGroupKey's. The arms written before these ran
	// the computed walk almost exclusively — the two exceptions prove nothing
	// about the nested route either, since a_bare_key_is_the_control drives the
	// FieldValue walk over a FLAT single-accessor key and
	// a_duplicate_computed_key never reaches a walk at all. None of them can
	// speak for the FieldValue walk over a fused multi-accessor root, which is
	// the one shape that walk never saw before.

	t.Run("a_nested_bare_key_reread_in_having_binds_its_own_slot", func(t *testing.T) {
		// The wrong-slot signature here is LOUD rather than silent, and the
		// reason is the same luck the nested computed arm above turns on: the
		// fused root R carries input ordinal 2 over [ID Q R] while the output
		// row [R.V.Z, MAX(Q.S)] is 2 wide, so an unbound read is out of range
		// and the executor refuses. Asserting VALUES rather than absence-of-
		// error is still what this arm does, because an out-of-range read is
		// only one of the two ways it can be wrong — binding the AGGREGATE's
		// slot would answer, with contradictory rows.
		//
		// Java answers these: the vendored corpus pins
		// `select max(q.s) from nested group by r.v.z having r.v.z > 120` at
		// `[{330}]` (groupby-tests.yamsql), which is the first assertion below.
		const why = "a NESTED bare grouping key must bind the aggregate output slot it " +
			"occupies. Its fused root carries input ordinal 2 over [ID Q R]; the output " +
			"row is [R.V.Z, MAX(Q.S)]."
		eq(t, "SELECT max(q.s) FROM nested GROUP BY r.v.z HAVING r.v.z > 120", 1, "[[330]]", why)
		// The mirrored direction, for the reason the flat arms carry it: an arm
		// that only over-admits cannot tell a wrong slot from a dropped predicate.
		eq(t, "SELECT max(q.s) FROM nested GROUP BY r.v.z HAVING r.v.z < 120", 1, "[[203]]", why)
		// Key and predicate in ONE result set, so no reading of the data can
		// make a wrong binding look correct.
		eq(t, "SELECT r.v.z, max(q.s) FROM nested GROUP BY r.v.z HAVING r.v.z > 120",
			2, "[[140 330]]", why)
	})

	t.Run("a_nested_key_in_a_nonzero_slot", func(t *testing.T) {
		// The slot-index axis for the NESTED route. two_computed_keys… above
		// establishes why this axis is necessary at all — a single-key arm
		// cannot tell the recorded loop index from a hardcoded 0 — but it drives
		// the computed walk only. This drives the FieldValue walk with a nested
		// key at index 1.
		//
		// Groups are the eight (q.t + 1, r.v.z) pairs: q.t is 1..8 and r.v.z is
		// 100 for ids 1-4, 140 for ids 5-8.
		//
		// The comparands are chosen so neither answer is reproducible from the
		// other slot, which the obvious fixture does NOT give you:
		//   r.v.z > 120  -> the four 140 groups; on slot 0 (2..9) it is empty.
		//   q.t + 1 > 4  -> five groups, and it CROSSES the r.v.z boundary
		//                   (one 100 group and all four 140 groups), so no
		//                   predicate evaluated against slot 1 can produce it.
		const slotIdx = "This arm pins the SLOT INDEX for a NESTED key. Reading the other " +
			"grouping key's slot changes this answer, which separates the recorded loop " +
			"index from a hardcoded 0."
		eq(t, "SELECT q.t + 1, r.v.z, max(q.s) FROM nested GROUP BY q.t + 1, r.v.z HAVING r.v.z > 120",
			3, "[[6 140 330] [7 140 329] [8 140 328] [9 140 327]]", slotIdx)
		eq(t, "SELECT q.t + 1, r.v.z, max(q.s) FROM nested GROUP BY q.t + 1, r.v.z HAVING q.t + 1 > 4",
			3, "[[5 100 203] [6 140 330] [7 140 329] [8 140 328] [9 140 327]]", slotIdx)
	})

	t.Run("a_duplicate_nested_key_is_refused_before_the_rebase", func(t *testing.T) {
		// The nested half of the negative result above — and it does NOT hold
		// for the same reason, which is the point of asserting it separately
		// rather than assuming the flat argument covers it.
		//
		// THE TWO GATES ARE DIFFERENT PREDICATES, and NEITHER route has a
		// structural guarantee — the computed one does not either, which an
		// earlier version of this comment got wrong. A NESTED key has a non-empty
		// Bare, so it takes the gate's `default` branch: groupKeysEquivalent, a
		// NAME-BASED comparison over the normalized (Bare, Qualifier) pair plus
		// the Qualified flag (plan_visitor.go). Meanwhile the SIBLING walk that
		// binds it matches with SemanticEqualsUnderAliasMap. The computed route's
		// gate arm is nominally semantic, but it is reached only when both keys
		// resolve through a weaker walk than the one that mints them, and it
		// returns false on `(c1 + 1)` vs `c1 + 1` even when reached. So on BOTH
		// routes the two sites decide identity by different means, and both
		// interlocks are MEASURED facts rather than structural ones.
		//
		// Measured: every spelling below is refused 42702 before reaching the
		// rebase, including the ones whose qualifier text differs (`r.v.z` vs
		// `nested.r.v.z` vs the alias `a.r.v.z`) — so the normalization behind
		// groupKeysEquivalent is peeling the source name before comparing.
		//
		// THE HAZARD THIS NAMES IS REAL, AND IT IS ALREADY ARMED — just not on
		// the single-source spellings below. A semantic twin whose normalized
		// (Bare, Qualifier) pair differs slips the name-based gate while still
		// matching the walk's semantic predicate. Adding a JOIN produces exactly
		// that, because the normalization is switched off when the query has
		// joins: see under_a_join_two_equal_keys_are_refused_42702_at_output_construction,
		// which pins that shape. Those spellings are now refused too, but by the
		// OUTPUT-CONSTRUCTION pull-up rather than by this name-based gate — so the
		// refusals asserted HERE remain a property of the SINGLE-SOURCE shape and
		// of the gate, and are not themselves evidence about any other shape.
		// The general guarantee is the construction pull-up's, not this gate's,
		// and it works by REFUSING THE STATEMENT rather than by keeping equal
		// keys away from the rebase — the rebase is downstream of it and simply
		// never runs on a plan that has already failed.
		for _, q := range []string{
			"SELECT max(q.s) FROM nested GROUP BY r.v.z, r.v.z HAVING r.v.z > 120",
			"SELECT max(q.s) FROM nested GROUP BY r.v.z, r.v.z",
			"SELECT max(q.s) FROM nested GROUP BY r.v.z, nested.r.v.z HAVING r.v.z > 120",
			"SELECT max(q.s) FROM nested AS a GROUP BY a.r.v.z, r.v.z HAVING r.v.z > 120",
			"SELECT r.v.z, nested.r.v.z, max(q.s) FROM nested GROUP BY r.v.z, nested.r.v.z",
		} {
			_, err := db.QueryContext(ctx, q)
			if err == nil {
				t.Fatalf("%s now PLANS.\n"+
					"  Two semantically equal NESTED keys reaching the rebase makes the "+
					"sibling FieldValue walk's first-match loop able to choose a slot "+
					"silently, where Java raises AMBIGUOUS_COLUMN. Either restore the "+
					"refusal or make the rebase raise on a multi-match.", q)
			}
			if !strings.Contains(err.Error(), "42702") {
				t.Fatalf("%s: got %v, want 42702.\n"+
					"  The CODE is the point: this must be the duplicate-grouping-key "+
					"refusal (Java's AMBIGUOUS_COLUMN), not an unrelated rejection that "+
					"happens to keep the shape away from the rebase — and in particular "+
					"not the 0AF00 nested-key refusal RFC-230 retired.", q, err)
			}
		}
	})

	t.Run("a_parenthesised_computed_key_twin_is_NOT_refused_either", func(t *testing.T) {
		// THE COMPUTED DUPLICATE GATE IS ALSO ONLY MEASURED, NOT STRUCTURAL —
		// and this arm is why the arm above no longer claims otherwise. One pair
		// of parentheses defeats it, on a PLAIN TABLE, with no join and no
		// derived source.
		//
		// THE MECHANISM, INSTRUMENTED RATHER THAN INFERRED. Running the gate
		// with the walk results printed:
		//
		//   GROUP BY (c1 + 1), c1 + 1
		//     walk[0] err=<nil> val=*values.RecordConstructorValue
		//     walk[1] err=<nil> val=*values.ArithmeticValue
		//
		// BOTH walks SUCCEED, so the gate's SEMANTIC arm is the one that runs —
		// and it still returns false, because `(c1 + 1)` resolves to a
		// one-element RecordConstructorValue WRAPPING the arithmetic while
		// `c1 + 1` resolves to the bare ArithmeticValue. Semantically the same
		// expression, structurally two different Value types. So the gate is not
		// defeated by falling back to a weaker predicate here; it is defeated
		// while running its STRONGEST one.
		//
		// THE NON-UNWRAP IS CORRECT — do not "fix" it, and do not read this arm
		// as asking for that. Java's visitRecordConstructor does not unwrap a
		// one-element constructor either (ExpressionVisitor.java:918-925), which
		// is exactly why walkRecordConstructorInner does not; unwrapping in the
		// walk collapses the projection case and reintroduces a divergence Go
		// already fixed. (walk.go's summary list still says "1-element unnamed →
		// unwrap"; that line is stale and contradicts its own function. It is
		// what an earlier version of this comment cited.)
		//
		// MEASURED AGAINST THE LIVE JVM, because the natural inference from the
		// above is that Java sees the same asymmetry and also declines to refuse
		// — and that inference is WRONG. conformance's DuplicateGroupByJavaProbe
		// (paren_twin_*) reports Java refusing with `Ambiguous columns for
		// q…._0.AMOUNT + @c12`, naming the UNWRAPPED arithmetic: Java's
		// Expressions.pullUp compares FieldPath DERIVATIONS and descends through
		// the wrapper, finding the same one twice. Both engines build the
		// wrapper; only Java's guard looks past it. That measurement is what
		// makes this a divergence rather than conformance, and it is pinned
		// there in both directions.
		//
		// THE COMPARISON SPELLING FAILS DIFFERENTLY, and is worth pinning beside
		// it because it is a second, independent route to the same place:
		//
		//   GROUP BY (c1 = 1), c1 = 1
		//     walk[0] err=unsupported shape: *antlrgen.BinaryComparisonPredicateContext
		//     walk[1] err=unsupported shape: *antlrgen.BinaryComparisonPredicateContext
		//
		// The gate resolves with WalkExpression (posPredicate), which refuses to
		// fold a comparison to a Value, while the grouping key is MINTED with
		// WalkExpressionForProjection (posProjection), which folds it happily.
		// Both gate walks fail, so identity falls to the GetText token-stream
		// compare, where "(c1=1)" and "c1=1" differ and nothing is refused.
		//
		// NO MULTI-MATCH REACHES THE REBASE, and that is measured, not assumed.
		// Instrumenting the first-match loop to count matching keys reports
		// `matches=1` for every query here, over keys
		// [RecordConstructorValue, ArithmeticValue] — the same wrapper mismatch
		// that defeats the gate also keeps the two keys distinguishable at the
		// loop, so it binds the one correct slot. The loop's CONCLUSION holds;
		// what does not hold is the reason previously given for it.
		//
		// WHICH SPELLING CAN ARM THE LOOP, stated precisely because the obvious
		// reading is backwards. For the ARITHMETIC pair the loop can never be
		// armed by normalizing: walkAtom dispatches RecordConstructor the same
		// way in both positions, so any unwrap makes the GATE see AV vs AV and
		// refuse 42702 BEFORE the loop. It is the COMPARISON pair that carries
		// the hazard — its gate failure (posPredicate will not fold a comparison
		// at all) is independent of the wrapper, so it survives any unwrap.
		//
		// And the danger is DIRECTION-ASYMMETRIC, with the naive fix landing on
		// the wrong side:
		//   unwrap in WalkExpressionForProjection ONLY -> gate still compares
		//     RCV vs AV and does not refuse, while GroupKeys becomes [AV, AV] and
		//     a reference matches BOTH — ARMED;
		//   unwrap in WalkExpression ONLY (the gate's walk) -> gate sees AV vs AV
		//     -> 42702 — safe;
		//   unwrap in both, or beneath both -> safe.
		// The projection walk is the one that mints the planner's value, so it is
		// where someone would naturally change it. Do the loop guard first.
		//
		// BOUND AND OWNERSHIP: identical to the join divergence below. The rows
		// are arithmetically correct, so this is conformance-only — Go answers
		// where Java refuses.
		//
		// THE SMALL FIX DOES NOT CLOSE THIS ARM, and that asymmetry is the point
		// of pinning it separately from the join one. Making the rebase loops
		// raise on a multi-match closes the JOIN arm (its two equal FieldValue
		// keys measure `matches=2`), but here the keys are
		// [RecordConstructorValue, ArithmeticValue] and a reference matches only
		// ONE — `matches=1`, so no `>1` guard can ever fire. Closing this needs
		// the larger step: converge the duplicate gate's identity predicate with
		// the loops'. TODO.md Phase 12 lists what each step closes, so neither is
		// credited with the other's work and neither DONE criterion can be
		// satisfied by the wrong change.
		//
		// WHY THIS ARM STAYS GREEN UNDER THE m1 MUTATION (reverting the computed
		// walk), which would otherwise look like evidence it tests nothing: it
		// passes by COINCIDENCE. Without the walk the HAVING reference reads
		// output slot 1, which here holds `(c1+1)` — the key's own value — so
		// `> 120` separates 101 from 141 exactly as the correct binding does.
		// That is a property of this fixture, not a guard.
		//
		// IT DOES REDDEN UNDER THE OTHER MUTATION, and the two results only look
		// contradictory until the mutations are told apart: REVERTING the computed
		// walk (m1) leaves this green for the fixture reason above, while pinning
		// that walk's MINT to ordinal 0 reddens it, because the reference then
		// binds slot 0 — which holds `(c1+1)` as a RecordConstructorValue — rather
		// than slot 1. The second result is what proves this arm reaches the
		// COMPUTED walk at all.
		//
		// The DELIBERATE witness
		// for the computed walk's slot index is
		// two_computed_keys_bind_their_own_slot_and_not_slot_zero; this arm is an
		// incidental second one (its HAVING reference is an ArithmeticValue
		// matched against key index 1) and must not be read as replacing it.
		const why = "This arm pins a KNOWN Go-answers/Java-raises divergence, booked in " +
			"TODO.md Phase 12. If it now errors, check for 42702 and flip this arm to " +
			"assert the refusal — that is the fix landing, not a regression."
		// The keys are (101,101) and (141,141): duplicated, so the rows are the
		// same ones the single-key spelling returns. Both directions.
		eq(t, "SELECT max(c2) FROM flat GROUP BY (c1 + 1), c1 + 1", 1, "[[203] [330]]", why)
		eq(t, "SELECT max(c2) FROM flat GROUP BY (c1 + 1), c1 + 1 HAVING c1 + 1 > 120",
			1, "[[330]]", why)
		eq(t, "SELECT max(c2) FROM flat GROUP BY (c1 + 1), c1 + 1 HAVING c1 + 1 < 120",
			1, "[[203]]", why)
		// The comparison route, and a derived source, which the instrumentation
		// showed reaches the same place by the paren mechanism rather than by
		// buildSelectScope returning nil (the resolver was non-nil there).
		eq(t, "SELECT max(c2) FROM flat GROUP BY (c1 = 1), c1 = 1", 1, "[[330]]", why)
		eq(t, "SELECT max(c2) FROM (SELECT id, c1, c2 FROM flat) t "+
			"GROUP BY (c1 + 1), c1 + 1 HAVING c1 + 1 > 120", 1, "[[330]]", why)
		// THE CONTROL that keeps this arm honest: with the parentheses removed
		// the texts coincide and the refusal DOES fire, so what is pinned above
		// is the normalization hole and not a claim that the gate never works.
		for _, q := range []string{
			"SELECT max(c2) FROM flat GROUP BY c1 = 1, c1 = 1",
			"SELECT max(c2) FROM flat GROUP BY c1 + 1, c1 + 1",
		} {
			if _, err := db.QueryContext(ctx, q); err == nil ||
				!strings.Contains(err.Error(), "42702") {
				t.Fatalf("%s: got %v, want 42702 — the duplicate gate's control "+
					"regressed, which would mean the hole is wider than the "+
					"parenthesised spelling this arm pins", q, err)
			}
		}
	})

	t.Run("under_a_join_two_equal_keys_are_refused_42702_at_output_construction", func(t *testing.T) {
		// THE DIVERGENCE THIS ARM PINNED IS CLOSED, on all four spellings, and
		// WHERE it closes is the point.
		//
		// THE MECHANISM that made the shape reachable is unchanged: the name-based
		// duplicate gate normalizes by stripping a leading `aliasPrefix`, and that
		// prefix is computed only `if fs.tableAlias != "" && len(fs.joins) == 0` —
		// UNDER A JOIN THE STRIP IS OFF. So `a.r.v.z` keeps Qualifier `A.R.V` while
		// bare `r.v.z` keeps `R.V`, groupKeysEquivalent calls them different, and
		// the GATE still does not fire. That gate is untouched.
		//
		// WHAT REFUSES THEM IS THE OUTPUT-CONSTRUCTION PULL-UP, which is where
		// Java refuses them. LogicalOperator.java:454 pulls the grouping
		// expressions up against the GroupByExpression's own result value through
		// the ASSERTING Expressions.pullUp (Expressions.java:112), so two
		// semantically equal keys yield two entries for one key and `size() == 1`
		// raises AMBIGUOUS_COLUMN — at operator construction, before any
		// SELECT-list or HAVING pull-up, and independently of whether a
		// post-aggregate reference exists at all.
		//
		// THAT INDEPENDENCE IS WHY ALL FOUR MOVE TOGETHER NOW, and it corrects a
		// claim this file used to make. An earlier revision asserted that the two
		// PROJECTED spellings could not be closed by a pull-up guard because a
		// projected reference reaches none of the post-aggregate walks. The first
		// half is TRUE and instrumented — a projected key is bound by
		// buildAggregateOutputSlots, whose match is name-based and binds
		// `A.R.V.Z`→key 0 and `R.V.Z`→key 1, one match each. The inference was
		// false: the guard that closes them does not need a reference, because
		// the sub-expressions it pulls up are the GROUPING KEYS THEMSELVES.
		//
		// The same false inference had been written down as the reason the
		// `java_42702_go_plans` probes stay open ("a bare SELECT COUNT(*) carries
		// no post-aggregate reference for any pull-up to guard"). It was refuted
		// by this repo's own probe data in the same document:
		// `join_qualified_vs_bare` is a bare `SELECT COUNT(*)` and Java 42702s it.
		//
		// STILL OPEN, and NOT closed by this guard: the parenthesised computed
		// twin (the neighbouring arm). `(c1 + 1)` builds a RecordConstructorValue
		// and `c1 + 1` an ArithmeticValue, so no semantic matcher equates them at
		// construction either — that one needs the normalization step, not a
		// guard. Do not read this arm as closing it.
		for _, c := range []struct{ q, wantCol string }{
			// PROJECTED, nested and flat.
			{"SELECT a.r.v.z, r.v.z, max(a.q.s) FROM nested a JOIN flat b ON a.id = b.id " +
				"GROUP BY a.r.v.z, r.v.z", "A.R.V.Z"},
			{"SELECT b.c1, c1, max(b.c2) FROM nested a JOIN flat b ON a.id = b.id " +
				"GROUP BY b.c1, c1", "C1"},
			// HAVING, nested and flat.
			{"SELECT max(a.q.s) FROM nested a JOIN flat b ON a.id = b.id " +
				"GROUP BY a.r.v.z, r.v.z HAVING r.v.z > 120", "A.R.V.Z"},
			{"SELECT max(b.c2) FROM nested a JOIN flat b ON a.id = b.id " +
				"GROUP BY b.c1, c1 HAVING c1 > 120", "C1"},
			// NO REFERENCE AT ALL — the shape that proves the guard is at
			// construction and not on any reference path. Neither spelling
			// projects a key or carries a HAVING, so every post-aggregate walk is
			// inert here; only the construction pull-up can refuse them.
			{"SELECT max(a.q.s) FROM nested a JOIN flat b ON a.id = b.id " +
				"GROUP BY a.r.v.z, r.v.z", "A.R.V.Z"},
			{"SELECT count(*) FROM nested a JOIN flat b ON a.id = b.id " +
				"GROUP BY b.c1, c1", "C1"},
		} {
			_, err := db.QueryContext(ctx, c.q)
			if err == nil {
				t.Errorf("%s: planned, want 42702. The output-construction pull-up "+
					"stopped refusing two semantically equal grouping keys; Go is "+
					"answering a query Java declines at operator construction.", c.q)
				continue
			}
			if !strings.Contains(err.Error(), "42702") {
				t.Errorf("%s: got %v, want 42702", c.q, err)
			}
			// The named column says the refusal is ABOUT the duplicated grouping
			// key rather than an unrelated 42702 from elsewhere in the statement.
			// It deliberately does NOT claim to say which of the two equal keys
			// was taken: they denote the same column, so both render this name.
			// That distinction is unobservable here by construction, and pretending
			// otherwise is how a weak assertion gets read as a strong one.
			if !strings.Contains(err.Error(), c.wantCol) {
				t.Errorf("%s: got %v, want the message to name %q", c.q, err, c.wantCol)
			}
		}
	})
}
