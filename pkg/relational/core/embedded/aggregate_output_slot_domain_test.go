package embedded

// The translated ordinal's layout and owner.
//
// The hazard is documented from production one file over: a group key's
// SOURCE-relative ordinal and an aggregate's OUTPUT-row ordinal met in one
// comparison and matched because the integers coincided, rewriting the `SUM(v)`
// of `HAVING v > SUM(v)` into a reference to the group key `V`, after which the
// predicate looked key-only and was pushed onto the raw scan. Two ordinals are
// only comparable against a stated layout; these assertions are that the layout
// is now stated, and that it is the OUTPUT row's rather than the source's.
//
// Both directions are asserted separately and both matter:
//
//   - KNOWN. Mint no token and every consumer fails closed — a declined
//     optimization, silently, forever.
//   - the RIGHT one. A known-but-wrong token is strictly worse than none: it
//     makes the source-vs-output comparison that the element exists to refuse
//     succeed, wearing a proof's clothes. So the expectation below is built from
//     the query's output column list written out by hand, never from the
//     production derivation, which would make the assertion a tautology.

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestPinnedAggregateReferenceStatesTheLayoutItsOrdinalIndexes(t *testing.T) {
	t.Parallel()

	const ddl = "CREATE TABLE t (k BIGINT, v BIGINT, w BIGINT, PRIMARY KEY (k))"

	cases := []struct {
		name string
		sql  string
		// outCols is the aggregate's native output row, [group keys...,
		// calls...], written out by hand — the layout every pinned ordinal in
		// this query must declare.
		outCols []string
		// srcCols is the SOURCE layout the same ordinals would index if the
		// composition had reported its own table instead. Asserting the token
		// differs from this is what makes "known" mean something.
		srcCols []string
		why     string
	}{
		{
			name:    "aggregate_call_reference",
			sql:     "SELECT k, SUM(v), SUM(w) FROM t GROUP BY k HAVING SUM(w) > 5",
			outCols: []string{"K", "SUM(V)", "SUM(W)"},
			srcCols: []string{"K", "V", "W"},
			why: "SUM(w) is slot 2 of the output row. Slot 2 of the SOURCE row is W — a " +
				"different column with the same integer, which is the whole conflation.",
		},
		{
			name: "group_key_reference_that_stays_above_the_aggregate",
			// The conjunction is load-bearing. A PURE group-key HAVING
			// (`HAVING v > 3`) is pushed BELOW the aggregate by
			// PushFilterThroughGroupByRule and stays source-relative, so it
			// carries no pinned reference at all. Mentioning an aggregate pins
			// the predicate above the group boundary, which is where a group-key
			// reference must read the OUTPUT row.
			sql:     "SELECT v, SUM(w) FROM t GROUP BY v HAVING v > 3 AND SUM(w) > 5",
			outCols: []string{"V", "SUM(W)"},
			srcCols: []string{"K", "V", "W"},
			why: "A group-key reference above the boundary is pinned to the output row too: " +
				"V is slot 0 there and slot 1 of the source. Same column, two layouts, " +
				"two integers.",
		},
		{
			name:    "the_colliding_shape",
			sql:     "SELECT v, SUM(w) FROM t GROUP BY v HAVING v > SUM(w)",
			outCols: []string{"V", "SUM(W)"},
			srcCols: []string{"K", "V", "W"},
			why: "The production hazard itself: the group key's source ordinal 1 and the " +
				"call's output ordinal 1 collide. One comparison, two layouts — and the " +
				"domain is the only thing that can refuse it.",
		},
		{
			name:    "no_group_key",
			sql:     "SELECT SUM(v), SUM(w) FROM t HAVING SUM(w) > 5",
			outCols: []string{"SUM(V)", "SUM(W)"},
			srcCols: []string{"K", "V", "W"},
			why: "Without a key the calls start at 0, so the layout is a different width " +
				"as well as a different content — a token derived from a hard-coded " +
				"key offset would not survive this.",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, having, owner := translatedHavingFor(t, tc.sql, ddl)
			want := values.OrdinalDomainOfColumnNames(tc.outCols)
			if !want.IsKnown() {
				t.Fatalf("test expectation %v is itself unknown", tc.outCols)
			}
			source := values.OrdinalDomainOfColumnNames(tc.srcCols)
			if want == source {
				t.Fatalf("test setup: output layout %v and source layout %v are the same "+
					"token, so this case cannot tell them apart", tc.outCols, tc.srcCols)
			}

			got := values.OrdinalDomainOfType(owner.FlowedType())
			if got != want {
				extra := ""
				if got == source {
					extra = " That is the SOURCE table's layout: the HAVING quantifier " +
						"owns the row consumed by the aggregate rather than the row it emits."
				}
				t.Errorf("%s: translated HAVING owner declares %v, want the aggregate's "+
					"native output layout %v (%v).%s\n%s",
					tc.sql, got, tc.outCols, want, extra, tc.why)
			}
			if slots := translatedSlots(t, having, owner); len(slots) == 0 {
				t.Fatalf("%s: translated HAVING predicate contains no aggregate-output FieldValue", tc.sql)
			}
		})
	}
}
