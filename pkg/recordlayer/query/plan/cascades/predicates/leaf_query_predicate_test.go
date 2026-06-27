package predicates

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestIsLeafQueryPredicate_True_OnLeaves(t *testing.T) {
	t.Parallel()
	cases := []QueryPredicate{
		NewConstantPredicate(TriTrue),
		NewValuePredicate(values.LiteralValue(int64(1))),
		NewComparisonPredicate(
			values.LiteralValue(int64(1)),
			Comparison{Type: ComparisonEquals, Operand: values.LiteralValue(int64(1))},
		),
		NewExistentialAlias(values.NamedCorrelationIdentifier("x")),
	}
	for i, p := range cases {
		if !IsLeafQueryPredicate(p) {
			t.Errorf("case %d: %T should be a leaf", i, p)
		}
	}
}

func TestIsLeafQueryPredicate_False_OnInner(t *testing.T) {
	t.Parallel()
	leaf := NewConstantPredicate(TriTrue)
	cases := []QueryPredicate{
		NewAnd(leaf, leaf),
		NewOr(leaf, leaf),
		NewNot(leaf),
	}
	for i, p := range cases {
		if IsLeafQueryPredicate(p) {
			t.Errorf("case %d: %T should NOT be a leaf", i, p)
		}
	}
}

func TestIsLeafQueryPredicate_NilSafe(t *testing.T) {
	t.Parallel()
	if IsLeafQueryPredicate(nil) {
		t.Fatal("nil should not be a leaf")
	}
}
