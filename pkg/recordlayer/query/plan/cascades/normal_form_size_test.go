package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
)

// TestNormalFormSize_IsNegateAware asserts the metric POSITIVELY, by named
// value, rather than by differential against the negate-blind walk it replaces
// — that walk is deleted, so a differential against it could only ever compare
// the new code to itself.
//
// The cost model reads this number: Java's countNormalizedConjuncts
// (NormalizedResidualPredicateProperty.java:81-90) is
// getMetrics(p).getNormalFormFullSize(), consumed at PlanningCostModel.java:142-143
// and RewritingCostModel.java:92-93. Go's designated_final.go and
// planning_cost_model.go are the ports of those readers.
//
// The load-bearing row is NOT(a OR b) under CNF. Under negation the Or is sized
// as a MAJOR, so its children SUM to 2; the retired walk recursed through the
// NOT without swapping roles and multiplied to 1. Every other row is here to
// show the swap is a swap and not a blanket increase.
func TestNormalFormSize_IsNegateAware(t *testing.T) {
	t.Parallel()
	a, b, c := normalFormTestLeaves(t)

	cases := []struct {
		name string
		pred predicates.QueryPredicate
		cnf  int64
		dnf  int64
	}{
		// A bare leaf, and a NOT over a leaf: no connective, so no swap.
		{"leaf", a, 1, 1},
		{"not-leaf", predicates.NewNot(a), 1, 1},

		// The positive forms. CNF majors (And) sum; CNF minors (Or) multiply.
		{"and", predicates.NewAnd(a, b), 2, 1},
		{"or", predicates.NewOr(a, b), 1, 2},
		{"and-of-ors", predicates.NewAnd(predicates.NewOr(a, b), predicates.NewOr(b, c)), 2, 4},

		// THE ROW THIS CHANGE IS ABOUT. Negated, the roles swap.
		{"not-or", predicates.NewNot(predicates.NewOr(a, b)), 2, 1},
		{"not-and", predicates.NewNot(predicates.NewAnd(a, b)), 1, 2},

		// A doubled NOT restores the un-negated sizing, which is what proves
		// the flag is carried rather than merely consulted once.
		{"not-not-or", predicates.NewNot(predicates.NewNot(predicates.NewOr(a, b))), 1, 2},

		// NOT over a nested shape: the swap recurses. Both expectations are
		// derived from the normal form itself rather than from walking the
		// code, because walking the code is how you re-assert whatever it
		// already does — an earlier draft of this row guessed 1 and 3 and both
		// were wrong.
		//
		//   NOT(a AND (b OR c))  ==  NOT a OR (NOT b AND NOT c)
		//     DNF majors (Or terms):  NOT a | (NOT b AND NOT c)          -> 2
		//     CNF, distributed:       (NOT a OR NOT b) AND (NOT a OR NOT c) -> 2
		{"not-and-of-or", predicates.NewNot(predicates.NewAnd(a, predicates.NewOr(b, c))), 2, 2},
	}

	swapped := 0
	for _, tc := range cases {
		if got := normalFormSize(tc.pred, false, normalFormCNF); got != tc.cnf {
			t.Errorf("%s: normalFormSize(%s, false, CNF) = %d, want %d",
				tc.name, tc.pred.Explain(), got, tc.cnf)
		}
		if got := normalFormSize(tc.pred, false, normalFormDNF); got != tc.dnf {
			t.Errorf("%s: normalFormSize(%s, false, DNF) = %d, want %d",
				tc.name, tc.pred.Explain(), got, tc.dnf)
		}
		if tc.cnf != tc.dnf {
			swapped++
		}
	}

	// Non-vacuity: if every row answered the same in both modes, the mode
	// parameter would be inert and this table would pass against a normalizer
	// that ignored it entirely.
	if swapped < 4 {
		t.Fatalf("only %d of %d rows distinguish CNF from DNF — the table does not "+
			"exercise the major/minor swap", swapped, len(cases))
	}
}

func normalFormTestLeaves(t *testing.T) (x, y, z predicates.QueryPredicate) {
	t.Helper()
	bld := &predicateBuilder{script: []byte{0}}
	col0 := bld.column(0)
	col1 := bld.column(1)
	return predicates.NewComparisonPredicate(col0,
			predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(1))),
		predicates.NewComparisonPredicate(col1,
			predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(2))),
		predicates.NewComparisonPredicate(col0,
			predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, int64(0)))
}
