package predicates

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestRangePredicateSemanticEquality(t *testing.T) {
	t.Parallel()
	a, b := values.NamedCorrelationIdentifier("a"), values.NamedCorrelationIdentifier("b")
	aliases := mustAliasMap(t, values.AliasPair{Source: a, Target: b})
	gt := Comparison{Type: ComparisonGreaterThan, Operand: values.LiteralValue(int64(7))}
	lt := Comparison{Type: ComparisonLessThan, Operand: values.LiteralValue(int64(20))}
	left := NewPredicateWithValueAndRanges(mustQOV(t, a), []*RangeConstraints{
		NewRangeConstraints([]Comparison{gt, lt}, nil),
		NewRangeConstraints(nil, []Comparison{{Type: ComparisonEquals, Operand: mustQOV(t, a)}}),
	})
	makeRight := func() *PredicateWithValueAndRanges {
		return NewPredicateWithValueAndRanges(mustQOV(t, b), []*RangeConstraints{
			NewRangeConstraints(nil, []Comparison{{Type: ComparisonEquals, Operand: mustQOV(t, b)}}),
			NewRangeConstraints([]Comparison{lt, gt}, nil),
		})
	}
	for _, tc := range []struct {
		name   string
		mutate func(*PredicateWithValueAndRanges)
		want   bool
	}{
		{"permuted_ranges_and_comparisons", func(*PredicateWithValueAndRanges) {}, true},
		{"duplicate_range", func(p *PredicateWithValueAndRanges) { p.ranges = append(p.ranges, p.ranges[0]) }, true},
		{"duplicate_comparison", func(p *PredicateWithValueAndRanges) { p.ranges[1] = NewRangeConstraints([]Comparison{lt, gt, gt}, nil) }, true},
		{"missing_range", func(p *PredicateWithValueAndRanges) { p.ranges = p.ranges[:1] }, false},
		{"missing_bound", func(p *PredicateWithValueAndRanges) { p.ranges[1] = NewRangeConstraints([]Comparison{gt}, nil) }, false},
		{"changed_value", func(p *PredicateWithValueAndRanges) { p.value = values.LiteralValue(int64(0)) }, false},
		{"unmapped_comparand", func(p *PredicateWithValueAndRanges) {
			p.ranges[0] = NewRangeConstraints(nil, []Comparison{{Type: ComparisonEquals, Operand: mustQOV(t, a)}})
		}, false},
		{"changed_literal", func(p *PredicateWithValueAndRanges) {
			changed := gt
			changed.Operand = values.LiteralValue(int64(8))
			p.ranges[1] = NewRangeConstraints([]Comparison{lt, changed}, nil)
		}, false},
		{"changed_operator", func(p *PredicateWithValueAndRanges) {
			changed := gt
			changed.Type = ComparisonGreaterThanEq
			p.ranges[1] = NewRangeConstraints([]Comparison{lt, changed}, nil)
		}, false},
		{"moved_to_deferred", func(p *PredicateWithValueAndRanges) { p.ranges[1] = NewRangeConstraints(nil, []Comparison{lt, gt}) }, false},
		{"empty_range", func(p *PredicateWithValueAndRanges) { p.ranges[1] = EmptyRangeConstraints() }, false},
		{"parameter_name", func(p *PredicateWithValueAndRanges) {
			changed := gt
			changed.ParameterName = "other"
			p.ranges[1] = NewRangeConstraints([]Comparison{lt, changed}, nil)
		}, false},
		{"text_metadata", func(p *PredicateWithValueAndRanges) {
			changed := gt
			changed.TextAnalyzerName = "other"
			p.ranges[1] = NewRangeConstraints([]Comparison{lt, changed}, nil)
		}, false},
		{"vector_knob", func(p *PredicateWithValueAndRanges) {
			changed := gt
			n := 17
			changed.EfSearch = &n
			p.ranges[1] = NewRangeConstraints([]Comparison{lt, changed}, nil)
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			right := makeRight()
			tc.mutate(right)
			if got := SemanticEqualsUnderAliasMap(left, right, aliases); got != tc.want {
				t.Errorf("semantic equality=%t, want %t", got, tc.want)
			}
			if tc.want && SemanticHashCode(left) != SemanticHashCode(right) {
				t.Error("alias-equivalent range predicates hash differently")
			}
			if PredicateEquals(left, right) {
				t.Error("unmapped aliases compared equal")
			}
		})
	}
	if !PredicateEquals(left, left.WithRanges(left.GetRanges())) {
		t.Error("independently built unchanged range predicate compares unequal")
	}
	if !SemanticEqualsUnderAliasMap(NewPredicateWithValueAndRanges(values.LiteralValue(1), nil), NewPredicateWithValueAndRanges(values.LiteralValue(1), nil), nil) {
		t.Error("two empty range sets must compare equal")
	}
	if SemanticEqualsUnderAliasMap(left, left, aliases) {
		t.Error("shared predicate bypassed a changed alias binding")
	}
}
