package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// A unique index equality on a ZERO float must NOT prove at-most-one: the
// executor widens it across both signed zeros and a unique index holds both.
func TestUniqueIndexZeroEqualityIsNotAtMostOne(t *testing.T) {
	t.Parallel()
	mk := func(lit any) *plans.RecordQueryIndexPlan {
		cr := predicates.EmptyComparisonRange()
		res := cr.Merge(&predicates.Comparison{
			Type: predicates.ComparisonEquals, Operand: values.LiteralValue(lit),
		})
		return plans.NewRecordQueryIndexPlan("IDX",
			[]*predicates.ComparisonRange{res.Range},
			[]string{"T"}, values.UnknownType, false,
		).WithIndexMetadata([]string{"V"}, []string{"ID"}, true)
	}
	nonZero := computeCardinalities(nil, mk(float64(5)))
	if nonZero.GetMaxCardinality().IsUnknown() {
		t.Fatalf("unique index, nonzero equality: max is unknown, want a proven bound of 1")
	}
	zero := computeCardinalities(nil, mk(float64(0)))
	if !zero.GetMaxCardinality().IsUnknown() {
		t.Fatalf("unique index, ZERO equality: max = %v, want UNKNOWN — the scan widens "+
			"across -0.0 and +0.0, a unique index holds both, so at-most-one is a FALSE proof",
			zero.GetMaxCardinality())
	}
}
