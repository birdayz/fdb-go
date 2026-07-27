package plans

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestFlatMapHintCost_LimitZeroInnerYieldsZeroCardinality is the end-to-end
// pin for the FlatMapCost zero-cardinality defect, built out of the actual
// plan nodes a `SELECT ... FROM outer, (SELECT ... FROM inner LIMIT 0)`
// query produces: a RecordQueryLimitPlan with limit=0 as the FlatMap's inner.
// RecordQueryLimitPlan.HintCost correctly reports Cardinality 0 for that
// LIMIT 0 (see limit.go); RecordQueryFlatMapPlan.HintCost must propagate that
// zero, not substitute LeafScanCardinality and report a 1e6x-inflated join.
func TestFlatMapHintCost_LimitZeroInnerYieldsZeroCardinality(t *testing.T) {
	t.Parallel()

	outerCost := properties.Cost{Cardinality: 1000, CPU: 100}

	limitPlan := NewRecordQueryLimitPlan(nil, 0, 0)
	limitCost := limitPlan.HintCost([]properties.Cost{{Cardinality: 5000, CPU: 500}}, nil)
	if limitCost.Cardinality != 0 {
		t.Fatalf("setup error: LIMIT 0 cost cardinality = %v, want 0", limitCost.Cardinality)
	}

	flatMap := NewRecordQueryFlatMapPlan(nil, nil, values.CorrelationIdentifier{}, values.CorrelationIdentifier{}, nil, false)
	got := flatMap.HintCost([]properties.Cost{outerCost, limitCost}, nil)
	if got.Cardinality != 0 {
		t.Fatalf("FlatMap over LIMIT-0 inner cardinality = %v, want 0 (outerCard=%v would make LeafScanCardinality substitution %v)",
			got.Cardinality, outerCost.Cardinality, outerCost.Cardinality*properties.LeafScanCardinality)
	}
}

// TestRecursiveCost_ZeroSeedCardinalityStaysZero pins the same
// zero-as-sentinel fix applied to recursiveCost (plan/plans/cost.go),
// audited alongside FlatMapCost/NestedLoopJoinCost for the identical
// conflation: a LIMIT-0 recursive-CTE seed leg must recurse zero times and
// produce a zero-row CTE, not LeafScanCardinality*recCard rows conjured from
// a seed that produced none.
func TestRecursiveCost_ZeroSeedCardinalityStaysZero(t *testing.T) {
	t.Parallel()

	zeroSeed := properties.Cost{Cardinality: 0, CPU: 3}
	recLeg := properties.Cost{Cardinality: 50, CPU: 5}

	var zeroQ expressions.Quantifier
	dfs := NewRecordQueryRecursiveDfsJoinPlanFromQuantifiers(zeroQ, zeroQ, values.CorrelationIdentifier{}, DfsPreorder, false)
	got := dfs.HintCost([]properties.Cost{zeroSeed, recLeg}, nil)
	if got.Cardinality != 0 {
		t.Fatalf("Cardinality = %v, want 0 (empty seed recurses zero times)", got.Cardinality)
	}
}
