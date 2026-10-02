package predicates

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestRangeConstraintsBuilder_JavaLanesAndSets(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		comparison Comparison
		compiled   bool
	}{
		{"literal", NewLiteralComparison(ComparisonEquals, int64(2)), true},
		{"null", Comparison{Type: ComparisonIsNull}, true},
		{"parameter", Comparison{Type: ComparisonEquals, ParameterName: "p"}, false},
		{"prefix", NewLiteralComparison(ComparisonStartsWith, "abc"), false},
		{"distinct", NewLiteralComparison(ComparisonIsDistinctFrom, int64(2)), false},
		{"arithmetic", Comparison{Type: ComparisonEquals, Operand: &values.ArithmeticValue{Op: values.OpAdd, Left: values.LiteralValue(int64(1)), Right: values.LiteralValue(int64(2))}}, false},
		{"correlated", Comparison{Type: ComparisonEquals, Operand: mustQOV(t, values.NamedCorrelationIdentifier("outer"))}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := NewRangeConstraintsBuilder()
			for range 2 {
				if !b.AddComparisonMaybe(tc.comparison) {
					t.Fatal("comparison must be admitted")
				}
			}
			r := b.Build()
			if got := len(r.GetComparisons()); got != 1 {
				t.Fatalf("comparison set contains %d repeated members, want 1", got)
			}
			if got := len(r.GetCompilableComparisons()) == 1; got != tc.compiled {
				t.Fatalf("compiled=%v, want %v", got, tc.compiled)
			}
			copy := NewRangeConstraintsBuilder()
			copy.Add(r)
			copy.Add(r)
			if len(copy.Build().GetComparisons()) != 1 || len(copy.Build().GetDeferredRanges()) != len(r.GetDeferredRanges()) {
				t.Fatal("range intersection must preserve lanes and set membership")
			}
		})
	}
}

func TestRangePredicateSimplification_NullAndUnknownConjunction(t *testing.T) {
	t.Parallel()
	unknown := mustQOV(t, values.NamedCorrelationIdentifier("unknown"))
	for _, kind := range []ComparisonType{ComparisonEquals, ComparisonNotEquals, ComparisonLessThan, ComparisonLessThanOrEq, ComparisonGreaterThan, ComparisonGreaterThanEq, ComparisonStartsWith} {
		t.Run(kind.Symbol(), func(t *testing.T) {
			t.Parallel()
			for _, lhs := range []values.Value{values.NewNullValue(values.NullableLong), unknown} {
				rhs := values.Value(values.NewNullValue(values.NullableLong))
				if lhs != unknown {
					rhs = unknown
				}
				p := NewPredicateWithValueAndRanges(lhs, []*RangeConstraints{NewRangeConstraints(nil, []Comparison{{Type: kind, Operand: rhs}})})
				folded := SimplifyPredicateValues(p)
				constant, ok := folded.(*ConstantPredicate)
				if !ok || constant.Value != TriUnknown {
					t.Fatalf("NULL comparison must fold even with unknown peer: %T %v", folded, folded)
				}
			}
		})
	}
	p := NewPredicateWithValueAndRanges(values.NewNullValue(values.NullableLong), []*RangeConstraints{
		NewRangeConstraints(nil, []Comparison{{Type: ComparisonIsNotNull}, {Type: ComparisonIsDistinctFrom, Operand: unknown}}),
	})
	folded := SimplifyPredicateValues(p)
	constant, ok := folded.(*ConstantPredicate)
	if !ok || constant.Value != TriFalse {
		t.Fatalf("FALSE range conjunct must dominate an unknown conjunct: %T %v", folded, folded)
	}
}

func TestPartitionPredicatesPreservesMalformedInputsForAdmission(t *testing.T) {
	t.Parallel()
	var nilValue *values.ConstantValue
	v := values.LiteralValue(int64(1))
	for _, tc := range []struct {
		name      string
		predicate QueryPredicate
	}{
		{"nil_predicate", nil},
		{"typed_nil_predicate", (*ComparisonPredicate)(nil)},
		{"typed_nil_value", NewComparisonPredicate(nilValue, NewLiteralComparison(ComparisonEquals, int64(1)))},
		{"typed_nil_comparand", NewComparisonPredicate(v, Comparison{Type: ComparisonEquals, Operand: nilValue})},
		{"nil_range", NewPredicateWithValueAndRanges(v, []*RangeConstraints{nil})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := PartitionPredicates([]QueryPredicate{tc.predicate})
			if len(got) != 1 || got[0] != tc.predicate {
				t.Fatalf("malformed predicate was rewritten or dropped before admission: %v", got)
			}
		})
	}
}

func TestPartitionPredicatesNestedBooleanFlattening(t *testing.T) {
	t.Parallel()
	v := mustQOV(t, values.NamedCorrelationIdentifier("x"))
	a := NewComparisonPredicate(v, NewLiteralComparison(ComparisonEquals, int64(1)))
	b := NewComparisonPredicate(v, NewLiteralComparison(ComparisonEquals, int64(2)))
	c := NewComparisonPredicate(v, NewLiteralComparison(ComparisonEquals, int64(3)))
	atomic := WithAtomicity(NewOr(a, NewOr(b, c)), true)
	nested := NewOr(a, NewOr(b, NewAnd(c, NewAnd(a, b))), atomic)
	got := PartitionPredicates([]QueryPredicate{nested})
	if len(got) != 1 {
		t.Fatalf("partition count = %d, want one disjunction", len(got))
	}
	or, ok := got[0].(*OrPredicate)
	if !ok || len(or.SubPredicates) != 4 {
		t.Fatalf("nested OR was not flattened: %v", got)
	}
	and, ok := or.SubPredicates[2].(*AndPredicate)
	if !ok || len(and.SubPredicates) != 3 {
		t.Fatalf("nested AND inside OR was not flattened: %v", or)
	}
	if or.SubPredicates[0] != a || or.SubPredicates[1] != b || or.SubPredicates[3] != atomic {
		t.Fatal("boolean flattening changed order or traversed an atomic barrier")
	}
}

func TestRangePredicateFoldingNonNullableUnknown(t *testing.T) {
	t.Parallel()
	v, err := values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("not_null"), values.NotNullLong)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		kind ComparisonType
		want TriBool
	}{{ComparisonIsNull, TriFalse}, {ComparisonIsNotNull, TriTrue}} {
		p := NewPredicateWithValueAndRanges(v, []*RangeConstraints{NewRangeConstraints([]Comparison{{Type: tc.kind}}, nil)})
		got := SimplifyPredicateValues(p)
		constant, ok := got.(*ConstantPredicate)
		if !ok || constant.Value != tc.want {
			t.Errorf("non-nullable unknown %s = %v, want %v", tc.kind.Symbol(), got, tc.want)
		}
	}
}
