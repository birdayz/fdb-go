package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestComparisonRowCorrelated pins the correlated-intersection guard's signal
// (RFC-069 review): a bound comparison disqualifies its leg from a
// primary-key intersection ONLY when its RHS depends on a per-row OUTER
// quantifier. Constant operands — plain ConstantValue literals AND constant-pool
// ConstantObjectValue references — must NOT be flagged, or the guard would
// over-exclude legitimate local multi-index intersections.
func TestComparisonRowCorrelated(t *testing.T) {
	t.Parallel()

	outer := values.NamedCorrelationIdentifier("c")
	constAlias := values.NamedCorrelationIdentifier("__const0")
	rowType := values.NewRecordType("correlated_intersection_row", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "X", FieldType: values.NullableString, Ordinal: 1},
	})
	outerValue, err := values.NewQuantifiedObjectValue(outer, rowType)
	if err != nil {
		t.Fatalf("construct exact outer value: %v", err)
	}
	outerField, err := values.ResolveFieldOrdinals(outerValue, []int{0})
	if err != nil {
		t.Fatalf("resolve outer field: %v", err)
	}
	constantComposite := values.NewCastValue(
		values.NewConstantObjectValue(constAlias, "const0", values.NotNullString),
		values.NullableString,
	)
	if _, correlated := values.GetCorrelatedToOfValue(constantComposite)[constAlias]; !correlated {
		t.Fatal("fixture: composite operand does not expose its nested constant-pool alias")
	}

	cases := []struct {
		name    string
		operand values.Value
		want    bool
	}{
		{
			name:    "literal_constant_not_correlated",
			operand: &values.ConstantValue{Value: "cancelled", Typ: values.NotNullString},
			want:    false,
		},
		{
			name:    "constant_pool_object_not_correlated",
			operand: values.NewConstantObjectValue(constAlias, "const0", values.NotNullString),
			want:    false,
		},
		{
			name:    "outer_quantifier_is_correlated",
			operand: outerValue,
			want:    true,
		},
		{
			name:    "field_over_outer_quantifier_is_correlated",
			operand: outerField,
			want:    true,
		},
		{
			name:    "composite_over_constant_object_not_correlated",
			operand: constantComposite,
			want:    false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := &predicates.Comparison{Type: predicates.ComparisonEquals, Operand: tc.operand}
			if got := comparisonRowCorrelated(c); got != tc.want {
				t.Fatalf("comparisonRowCorrelated(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}

	t.Run("nil_comparison", func(t *testing.T) {
		t.Parallel()
		if comparisonRowCorrelated(nil) {
			t.Fatal("nil comparison must not be flagged correlated")
		}
	})
}
