package embedded

// The pinned ordinal's THIRD element: which layout it indexes.
//
// aggregate_output_slot_recorded_test.go pins that a post-aggregate reference
// carries the ordinal its composition chose. That is two thirds of an identity.
// The remaining third is the layout the integer addresses, and without it the
// recorded slot is an integer no consumer may act on — which is why the
// `FrontierPinned` provenance bit was standing in for a domain at every
// downstream comparison.
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

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// pinnedDomains collects, for every PINNED flat FieldValue in a predicate's
// value trees, its display name and the domain token its path carries.
func pinnedDomains(t *testing.T, pred predicates.QueryPredicate) map[string]values.OrdinalDomain {
	t.Helper()
	got := map[string]values.OrdinalDomain{}
	visitValue := func(v values.Value) {
		values.WalkValue(v, func(n values.Value) bool {
			fv, ok := n.(*values.FieldValue)
			if !ok || fv.Resolved == nil || !fv.Resolved.FrontierPinned {
				return true
			}
			got[fv.Field] = fv.Resolved.Domain
			return true
		})
	}
	var visit func(predicates.QueryPredicate)
	visit = func(p predicates.QueryPredicate) {
		switch x := p.(type) {
		case *predicates.ComparisonPredicate:
			visitValue(x.Operand)
			visitValue(x.Comparison.Operand)
		case *predicates.AndPredicate:
			for _, s := range x.SubPredicates {
				visit(s)
			}
		case *predicates.OrPredicate:
			for _, s := range x.SubPredicates {
				visit(s)
			}
		case *predicates.NotPredicate:
			visit(x.Child)
		}
	}
	visit(pred)
	return got
}

func TestPinnedAggregateReferenceStatesTheLayoutItsOrdinalIndexes(t *testing.T) {
	t.Parallel()

	const ddl = "CREATE TABLE t (k BIGINT NOT NULL, v BIGINT, w BIGINT, PRIMARY KEY (k))"

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
			agg := aggregateFor(t, tc.sql, ddl)
			if agg.HavingPredicate == nil {
				t.Fatalf("%s: no HAVING predicate to inspect", tc.sql)
			}
			want := values.OrdinalDomainOfColumnNames(tc.outCols)
			if !want.IsKnown() {
				t.Fatalf("test expectation %v is itself unknown", tc.outCols)
			}
			source := values.OrdinalDomainOfColumnNames(tc.srcCols)
			if want == source {
				t.Fatalf("test setup: output layout %v and source layout %v are the same "+
					"token, so this case cannot tell them apart", tc.outCols, tc.srcCols)
			}

			got := pinnedDomains(t, agg.HavingPredicate)
			if len(got) == 0 {
				t.Fatalf("%s: no pinned reference in the HAVING predicate at all — the "+
					"composition-recorded slot is what carries the domain", tc.sql)
			}
			for field, domain := range got {
				if !domain.IsKnown() {
					t.Errorf("%s: pinned reference %q carries %v.\n%s\n"+
						"A pinned ordinal says \"this slot is a recorded fact\"; an unknown "+
						"domain says \"against an unstated row\". Together they are the "+
						"comparison FieldPath.Domain exists to refuse, and every consumer "+
						"downstream fails closed on it — silently.",
						tc.sql, field, domain, tc.why)
					continue
				}
				if domain != want {
					extra := ""
					if domain == source {
						extra = " That is the SOURCE table's layout: the ordinal is being " +
							"reported against the row the reference was resolved from " +
							"rather than the row the composition numbered it against."
					}
					t.Errorf("%s: pinned reference %q declares %v, want the aggregate's "+
						"native output layout %v (%v).%s\n%s",
						tc.sql, field, domain, tc.outCols, want, extra, tc.why)
				}
			}
		})
	}
}
