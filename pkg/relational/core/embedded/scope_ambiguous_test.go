package embedded

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestHasNonInnerConjunct pins the Case-1 polarity flag's CLASSIFICATION
// semantics deterministically: any conjunct not referencing an inner source
// flags — outer-only correlations AND reference-free filterables — EXCEPT a
// statically-TRUE leaf (routing TRUE outer is a no-op; flagging it
// over-declined semantics-neutral tautologies). An OR is ONE conjunct
// classified atomically: an inner ref ANYWHERE in it means the routing
// keeps it inner-side, so it must not flag.
func TestHasNonInnerConjunct(t *testing.T) {
	t.Parallel()

	innerRef := func(name, col string) predicates.QueryPredicate {
		return predicates.NewComparisonPredicate(
			qualRecTestField(t, name, col),
			predicates.Comparison{Type: predicates.ComparisonEquals, Operand: values.LiteralValue(int64(1))},
		)
	}
	constCmp := func(l, r int64) predicates.QueryPredicate {
		return predicates.NewComparisonPredicate(
			values.LiteralValue(l),
			predicates.Comparison{Type: predicates.ComparisonEquals, Operand: values.LiteralValue(r)},
		)
	}
	inner := map[string]struct{}{"M2": {}, "ST": {}}

	for _, tc := range []struct {
		name string
		pred predicates.QueryPredicate
		want bool
	}{
		{"nil_never_flags", nil, false},
		{"inner_ref_not_flagged", innerRef("M2", "C"), false},
		{"outer_ref_flags", innerRef("OT", "K"), true},
		{"reffree_contradiction_flags", constCmp(1, 0), true},
		{"reffree_tautology_not_flagged", constCmp(1, 1), false},
		{"and_mixed_flags_on_the_noninner_part", predicates.NewAnd(innerRef("M2", "C"), innerRef("OT", "K")), true},
		{"and_inner_plus_tautology_not_flagged", predicates.NewAnd(innerRef("M2", "C"), constCmp(1, 1)), false},
		// An OR is one conjunct, classified atomically: the inner ref
		// anywhere inside keeps it inner-routed.
		{"or_with_inner_ref_not_flagged", predicates.NewOr(innerRef("M2", "C"), innerRef("OT", "K")), false},
		{"or_all_noninner_flags", predicates.NewOr(innerRef("OT", "K"), constCmp(1, 0)), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := hasNonInnerConjunct(tc.pred, inner); got != tc.want {
				t.Fatalf("hasNonInnerConjunct(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}
