package cascades

import (
	"math"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func TestPhysicalEqualityConcreteCost_AgreesWithPlanAndProperty(t *testing.T) {
	t.Parallel()
	comps := []*predicates.ComparisonRange{pkGateEq(t, float64(0)), pkGateEq(t, float32(0))}
	types := []values.Type{values.NotNullDouble, values.NotNullFloat}
	pk := []values.Value{
		&values.FieldValue{Field: "V1", Typ: values.NotNullDouble},
		&values.FieldValue{Field: "V2", Typ: values.NotNullFloat},
	}
	plan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false).
		WithPrimaryKey(pk).
		WithScanComparisons(comps).
		WithKeyComponentTypes(types)
	stats := properties.FixedStatistics{Cardinality: 1000}

	hint := plan.HintCost(nil, stats)
	concrete := concretePlanCost(plan, stats, nil)
	if concrete != hint {
		t.Fatalf("concrete cost = %+v, plan HintCost = %+v; both must consume one physical shape", concrete, hint)
	}
	if concrete.Cardinality != 4 {
		t.Fatalf("concrete cardinality = %v, want four signed-zero key combinations", concrete.Cardinality)
	}
	bound := plan.ProvenCardinalities(nil).Max
	if bound.IsUnknown() || bound.Value() != 4 {
		t.Fatalf("property max = %+v, want 4", bound)
	}
	for name, proof := range map[string]func() (float64, bool){
		"logical data-access walk":  func() (float64, bool) { return scanProvableMaxCard(plan) },
		"concrete data-access walk": func() (float64, bool) { return scanPlanProvableMaxCard(plan, nil) },
	} {
		name, proof := name, proof
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			max, known := proof()
			if !known || max != 4 {
				t.Fatalf("max = (%v,%v), want (4,true)", max, known)
			}
		})
	}
}

func TestPhysicalEqualityScanLikeCost_DynamicAndOverflowRemainFinite(t *testing.T) {
	t.Parallel()
	dynamic := &values.ParameterValue{Ordinal: 1, Typ: values.UnknownType}
	comparison := predicates.Comparison{Type: predicates.ComparisonEquals, Operand: dynamic}
	merged := predicates.EmptyComparisonRange().Merge(&comparison)
	if !merged.Ok {
		t.Fatal("failed to build dynamic equality")
	}
	comps := []*predicates.ComparisonRange{merged.Range, pkGateEq(t, int64(5))}
	types := []values.Type{values.NotNullDouble, values.NotNullLong}
	cost := scanLikeCost(
		comps, types, []string{"T"}, properties.FixedStatistics{Cardinality: 1000}, true,
	)
	if cost.Cardinality == 1 || cost.Cardinality == 2 {
		t.Fatalf("dynamic float cardinality = %v, must remain a conservative estimate, not a point/fanout proof", cost.Cardinality)
	}
	selectivity, _, _ := properties.BoundSelectivity(comps)
	baseCard := 1000 * selectivity
	wantCPU := properties.SaturatingHeuristicAdd(
		properties.SaturatingHeuristicMultiply(
			properties.SaturatingHeuristicMultiply(baseCard, properties.ScanCPU),
			physicalWrapperCostMultiplier,
		),
		properties.PhysicalRangeSeekCost,
	)
	if cost.Cardinality != baseCard || cost.CPU != wantCPU {
		t.Fatalf("dynamic composite cost = %+v, want card=%v CPU=%v", cost, baseCard, wantCPU)
	}

	const count = 64
	zeros := make([]*predicates.ComparisonRange, count)
	zeroTypes := make([]values.Type, count)
	for i := range zeros {
		zeros[i] = pkGateEq(t, float64(0))
		zeroTypes[i] = values.NotNullDouble
	}
	overflow := scanLikeCost(
		zeros, zeroTypes, []string{"T"}, properties.FixedStatistics{Cardinality: 1000}, true,
	)
	if math.IsNaN(overflow.Cardinality) || math.IsInf(overflow.Cardinality, 0) ||
		math.IsNaN(overflow.CPU) || math.IsInf(overflow.CPU, 0) {
		t.Fatalf("overflow cost is non-finite: %+v", overflow)
	}
	if overflow.Cardinality != 0 && overflow.Cardinality > properties.MaxFiniteHeuristic {
		t.Fatalf("overflow cardinality escaped finite ceiling: %+v", overflow)
	}
	if overflow.CPU != properties.MaxFiniteHeuristic {
		t.Fatalf("overflow CPU = %v, want finite saturation %v", overflow.CPU, properties.MaxFiniteHeuristic)
	}
}
