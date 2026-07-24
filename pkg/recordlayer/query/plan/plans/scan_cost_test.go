package plans

import (
	"math"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func scanCostRange(t *testing.T, comparisonType predicates.ComparisonType, value any) *predicates.ComparisonRange {
	t.Helper()
	comparison := predicates.NewLiteralComparison(comparisonType, value)
	merge := predicates.EmptyComparisonRange().Merge(&comparison)
	if !merge.Ok {
		t.Fatalf("failed to build %v comparison range", comparisonType)
	}
	return merge.Range
}

// TestScanHintCost_PartialPKPrefixNotPointProbe pins RFC-190.3: scan
// comparisons contain only the bound primary-key prefix, so one equality
// comparison does not prove a point lookup for a two-column primary key.
func TestScanHintCost_PartialPKPrefixNotPointProbe(t *testing.T) {
	t.Parallel()

	stats := properties.FixedStatistics{Cardinality: 1000}
	pk2 := []values.Value{
		&values.FieldValue{Field: "TENANT", Typ: values.UnknownType},
		&values.FieldValue{Field: "ORDER", Typ: values.UnknownType},
	}
	eqTenant := scanCostRange(t, predicates.ComparisonEquals, int64(7))
	eqOrder := scanCostRange(t, predicates.ComparisonEquals, int64(9))
	rangeOrder := scanCostRange(t, predicates.ComparisonGreaterThan, int64(9))

	tests := []struct {
		name            string
		plan            *RecordQueryScanPlan
		wantCardinality float64
	}{
		{
			name: "composite PK partial equality prefix",
			plan: NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false).
				WithPrimaryKey(pk2).
				WithScanComparisons([]*predicates.ComparisonRange{eqTenant}),
			wantCardinality: stats.Cardinality *
				properties.EqualityBoundSelectivity *
				properties.PhysicalWrapperCostMultiplier,
		},
		{
			name: "composite PK fully equality bound",
			plan: NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false).
				WithPrimaryKey(pk2).
				WithScanComparisons([]*predicates.ComparisonRange{eqTenant, eqOrder}),
			wantCardinality: 1,
		},
		{
			name: "primary key metadata unavailable",
			plan: NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false).
				WithScanComparisons([]*predicates.ComparisonRange{eqTenant}),
			wantCardinality: stats.Cardinality *
				properties.EqualityBoundSelectivity *
				properties.PhysicalWrapperCostMultiplier,
		},
		{
			name: "full primary key includes range",
			plan: NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false).
				WithPrimaryKey(pk2).
				WithScanComparisons([]*predicates.ComparisonRange{eqTenant, rangeOrder}),
			wantCardinality: stats.Cardinality *
				properties.EqualityBoundSelectivity *
				properties.RangeSelectivity *
				properties.PhysicalWrapperCostMultiplier,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := test.plan.HintCost(nil, stats)
			if math.Abs(got.Cardinality-test.wantCardinality) > 1e-12 {
				t.Fatalf("scan cardinality = %v, want %v", got.Cardinality, test.wantCardinality)
			}
		})
	}
}
