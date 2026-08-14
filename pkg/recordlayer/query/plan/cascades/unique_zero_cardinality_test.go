package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// A unique index equality on a ZERO float proves the exact physical maximum
// two: the executor reads both signs and a unique index can hold both keys.
func TestUniqueIndexZeroEqualityIsNotAtMostOne(t *testing.T) {
	t.Parallel()
	row := values.NewRecordType("UniqueZeroRow", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong},
		{Name: "V", FieldType: values.NullableDouble},
	})
	mk := func(lit any) *plans.RecordQueryIndexPlan {
		cr := predicates.EmptyComparisonRange()
		res := cr.Merge(&predicates.Comparison{
			Type: predicates.ComparisonEquals, Operand: values.LiteralValue(lit),
		})
		plan, err := plans.NewRecordQueryIndexPlan("IDX",
			[]*predicates.ComparisonRange{res.Range},
			[]string{"T"}, row, false,
		)
		if err != nil {
			t.Fatalf("NewRecordQueryIndexPlan: %v", err)
		}
		return plan.WithKeyComponentTypes([]values.Type{values.NullableDouble}).
			WithIndexMetadata([]string{"V"}, []string{"ID"}, true)
	}
	nonZero := computeCardinalities(nil, mk(float64(5)))
	if nonZero.GetMaxCardinality().IsUnknown() {
		t.Fatalf("unique index, nonzero equality: max is unknown, want a proven bound of 1")
	}
	zero := computeCardinalities(nil, mk(float64(0)))
	if zero.GetMaxCardinality().IsUnknown() || zero.GetMaxCardinality().Value() != 2 {
		t.Fatalf("unique index, ZERO equality: max = %v, want 2 — the scan reads "+
			"-0.0 and +0.0 and a unique index can hold both physical keys",
			zero.GetMaxCardinality())
	}
}
