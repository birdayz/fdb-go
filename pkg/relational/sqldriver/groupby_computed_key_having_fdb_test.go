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
//   - two_computed_keys… — every other arm groups by ONE key, so the recorded
//     slot is always 0 and none of them can tell the loop index from a hardcoded
//     zero, while that index IS the binding. MEASURED: pinning the computed
//     walk's mint to ordinal 0 reddens ONLY that arm and leaves the other TWELVE
//     green, which is what says it tests the ordinal instead of re-testing the
//     walk. The sibling walk has its own such arm (a_nested_key_in_a_nonzero_slot)
//     because the two walks record the slot at two separate sites, and a mutation
//     of one leaves the other's arms untouched;
//   - a_duplicate_computed_key… — a NEGATIVE result. The rebase first-matches
//     where Java raises; nothing can present two matching keys because the
//     duplicate-key gate decides identity with the SAME predicate. The arm pins
//     the refusal that makes it unreachable and names what re-arms it.
//
// THREE FURTHER ARMS COVER THE NESTED ROUTE, which is a DIFFERENT walk. RFC-230
// retired the refusal that made a nested path unusable as a grouping key, so
// `GROUP BY r.v.z HAVING r.v.z > 120` became constructible at the same moment
// this file's fix changed how a re-read key binds; the two shapes had never run
// together. A nested key resolves to a fused multi-accessor FieldValue, so it is
// bound by the SIBLING FieldValue walk rather than by the computed walk the nine
// arms above drive — a route those arms cannot speak for. All three were
// measured CORRECT on first run; they are pins, not fixes. The duplicate arm is
// the sharpest of them: on the nested route the duplicate gate and the rebase
// matcher decide identity by DIFFERENT predicates (a string compare vs semantic
// equality), so the interlock that is structural for computed keys is only
// measured for nested ones.
//
// A THIRTEENTH ARM PINS A DIVERGENCE THIS FILE'S FIX DOES NOT CAUSE OR CLOSE.
// Chasing that string-vs-semantic asymmetry to ground found it is not merely a
// theoretical re-arm condition: the gate's qualifier normalization is switched
// off when the query has joins, so `… JOIN … GROUP BY a.r.v.z, r.v.z` puts two
// semantically equal keys in front of the sibling walk's first-match loop and
// Go answers where Java stops. under_a_join_two_equal_keys… pins it with values.
// It is PRE-EXISTING (the flat twin diverges identically, and this branch's
// delta over post-#719 master deletes NO source line — `git diff bd6f0c028 --
// pkg/relational/core/ | grep -c '^-[^-]'` is 0 — so it cannot have introduced
// or widened it) and BOUNDED to conformance rather than wrong rows: equal keys
// hold equal values, so first-match answers correctly.
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
		// EVERY OTHER ARM IN THIS FILE GROUPS BY ONE KEY, so the slot the
		// rebase records is always 0 and none of them can tell the loop index
		// from a hardcoded zero — while that index IS the binding. This arm
		// exists for the index alone.
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
		// THE INTERLOCK IS THAT BOTH SITES ASK THE SAME QUESTION. The duplicate
		// gate (plan_visitor.go, visitSelectGroupBy) decides key identity with
		// values.SemanticEqualsUnderAliasMap over the resolved expression
		// Values, and that is the SAME predicate rebasePostAggregateComputedGroupKey
		// matches with. Two keys that would both match a reference are therefore
		// semantically equal to each other, which is exactly what the gate
		// rejects. That shared predicate is what makes THIS arm's interlock
		// structural — and it is precisely what the NESTED arm below does not
		// have, so the two are asserted separately rather than one standing in
		// for the other.
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
	// rebasePostAggregateComputedGroupKey's. A file whose nine other arms all
	// drive the computed walk cannot speak for the FieldValue walk over a fused
	// multi-accessor root, which is the one shape that walk never saw before.

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
		// THE TWO GATES ARE DIFFERENT PREDICATES. For a COMPUTED key the
		// duplicate gate resolves both keys and compares them with
		// values.SemanticEqualsUnderAliasMap — the SAME predicate
		// rebasePostAggregateComputedGroupKey matches with, which is what makes
		// that arm's interlock airtight. A NESTED key has a non-empty Bare, so
		// it takes the gate's `default` branch instead: groupKeysEquivalent, a
		// STRING comparison over the normalized (Bare, Qualified, Qualifier)
		// triple (plan_visitor.go). Meanwhile the SIBLING walk that binds it
		// matches with SemanticEqualsUnderAliasMap. So on this route the two
		// sites decide identity by different means, and the interlock is a
		// MEASURED fact rather than a structural one.
		//
		// Measured: every spelling below is refused 42702 before reaching the
		// rebase, including the ones whose qualifier text differs (`r.v.z` vs
		// `nested.r.v.z` vs the alias `a.r.v.z`) — so the normalization behind
		// groupKeysEquivalent is peeling the source name before comparing.
		//
		// THE HAZARD THIS NAMES IS REAL, AND IT IS ALREADY ARMED — just not on
		// the single-source spellings below. A semantic twin whose normalized
		// (Bare, Qualifier) pair differs slips the string gate while still
		// matching the walk's semantic predicate. Adding a JOIN produces exactly
		// that, because the normalization is switched off when the query has
		// joins: see under_a_join_two_equal_keys_are_NOT_refused…, which pins it
		// with a reproducer. So the refusals asserted here are a property of the
		// SINGLE-SOURCE shape, and must not be read as a general guarantee that
		// two equal keys cannot reach the rebase.
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

	t.Run("under_a_join_two_equal_keys_are_NOT_refused_and_Go_answers_where_Java_raises", func(t *testing.T) {
		// A DIVERGENCE THIS CHANGE DOES NOT CAUSE AND DOES NOT FIX, pinned here
		// because it is the exact re-arm condition the two duplicate-key arms
		// above name, and it turns out to be CONSTRUCTIBLE.
		//
		// THE MECHANISM. Those arms hold because the duplicate gate normalizes
		// the qualifier before comparing: visitSelectGroupBy strips a leading
		// `aliasPrefix` so `r.v.z` and `nested.r.v.z` both reduce to (Z, R.V).
		// But that prefix is computed only `if fs.tableAlias != "" &&
		// len(fs.joins) == 0` — UNDER A JOIN THE STRIP IS OFF. So `a.r.v.z`
		// keeps Qualifier `A.R.V` while bare `r.v.z` keeps `R.V`,
		// groupKeysEquivalent compares the two strings, returns false, and no
		// 42702 fires. Two semantically equal keys then reach the first-match
		// loop in rebasePostAggregateGroupKeyValue, which binds the FIRST.
		//
		// JAVA REFUSES HERE, by two different mechanisms depending on the clause,
		// and the queries below deliberately cover both. The PROJECTED spellings
		// go through the SELECT-list variant Expressions.pullUp, which asserts
		// `pulledUpExpressionMap.get(subExpression).size() == 1` with
		// AMBIGUOUS_COLUMN (Expressions.java:112). The HAVING spellings go
		// through Expression.pullUp, which ends in a BARE
		// Iterables.getOnlyElement (Expression.java:246) with NO such assert — it
		// throws, but not with that SQLSTATE. So "Java raises AMBIGUOUS_COLUMN"
		// is precise only for the projected half; what both halves share is that
		// Java stops and Go answers.
		//
		// IT IS BOUNDED TO A CONFORMANCE DIVERGENCE, NOT WRONG ROWS, and that
		// bound is why it is pinned rather than fixed here: the two keys are
		// SEMANTICALLY EQUAL, so their two output slots hold identical values
		// and binding either answers correctly. The asserted rows below are the
		// arithmetically correct ones — Go returns the right answer to a query
		// Java declines to run.
		//
		// IT IS PRE-EXISTING AND NOT NESTED-SPECIFIC. The flat twin
		// (`GROUP BY b.c1, c1`) diverges identically, against the same sibling
		// walk, and this branch's source delta over post-#719 master is +130/-0 (a
		// PURE ADDITION: `git diff --stat bd6f0c028 -- pkg/relational/core/`, with
		// `grep -c '^-[^-]'` = 0) — a diff that deletes nothing cannot have
		// introduced or widened this.
		//
		// WHEN IT IS FIXED — by making the duplicate gate decide identity
		// semantically instead of by string, which is what removes the
		// asymmetry documented at rebasePostAggregateGroupKeyValue's first-match
		// loop — every query below must start returning 42702, and this arm
		// becomes that assertion. Until then it exists so the behaviour cannot
		// move in EITHER direction unnoticed: silently starting to refuse is a
		// change worth seeing too.
		const why = "This arm pins a KNOWN Go-answers/Java-raises divergence. If it now " +
			"errors, check the error is 42702 and flip this arm to assert the refusal — " +
			"that is the fix landing, not a regression."
		eq(t, "SELECT a.r.v.z, r.v.z, max(a.q.s) FROM nested a JOIN flat b ON a.id = b.id "+
			"GROUP BY a.r.v.z, r.v.z", 3, "[[100 100 203] [140 140 330]]", why)
		eq(t, "SELECT max(a.q.s) FROM nested a JOIN flat b ON a.id = b.id "+
			"GROUP BY a.r.v.z, r.v.z HAVING r.v.z > 120", 1, "[[330]]", why)
		// The FLAT twin, which is what says this is a property of the duplicate
		// gate under joins and not something nested paths brought with them.
		eq(t, "SELECT b.c1, c1, max(b.c2) FROM nested a JOIN flat b ON a.id = b.id "+
			"GROUP BY b.c1, c1", 3, "[[100 100 203] [140 140 330]]", why)
		eq(t, "SELECT max(b.c2) FROM nested a JOIN flat b ON a.id = b.id "+
			"GROUP BY b.c1, c1 HAVING c1 > 120", 1, "[[330]]", why)
	})
}
