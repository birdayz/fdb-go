package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// TestComparisonRowCorrelated pins the correlated-intersection guard's signal
// (RFC-069, Graefe review): a bound comparison disqualifies its leg from a
// primary-key intersection ONLY when its RHS depends on a per-row OUTER
// quantifier. Constant operands — plain ConstantValue literals AND constant-pool
// ConstantObjectValue references — must NOT be flagged, or the guard would
// over-exclude legitimate local multi-index intersections.
func TestComparisonRowCorrelated(t *testing.T) {
	t.Parallel()

	outer := values.NamedCorrelationIdentifier("c")
	constAlias := values.NamedCorrelationIdentifier("__const0")

	cases := []struct {
		name    string
		operand values.Value
		want    bool
	}{
		{
			name:    "literal_constant_not_correlated",
			operand: &values.ConstantValue{Value: "cancelled", Typ: values.UnknownType},
			want:    false,
		},
		{
			name:    "constant_pool_object_not_correlated",
			operand: values.NewConstantObjectValue(constAlias, "const0", values.UnknownType),
			want:    false,
		},
		{
			name:    "outer_quantifier_is_correlated",
			operand: values.NewQuantifiedObjectValue(outer),
			want:    true,
		},
		{
			name:    "field_over_outer_quantifier_is_correlated",
			operand: &values.FieldValue{Field: "ID", Typ: values.UnknownType, Child: values.NewQuantifiedObjectValue(outer)},
			want:    true,
		},
		{
			name: "field_over_constant_object_not_correlated",
			operand: &values.FieldValue{
				Field: "X", Typ: values.UnknownType,
				Child: values.NewConstantObjectValue(constAlias, "const0", values.UnknownType),
			},
			want: false,
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
