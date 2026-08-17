package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func TestPlanningCostModel_InUnionRepeatedFullScanCannotWinScalarFallback(t *testing.T) {
	t.Parallel()

	rowType := values.NewRecordType("CostRow", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
	})
	filteredScan := func() *plans.RecordQueryPredicatesFilterPlan {
		scan, err := plans.NewRecordQueryScanPlan([]string{"T"}, rowType, false)
		scan = mustConstruct(t, scan, err)
		filter, err := plans.NewRecordQueryPredicatesFilterPlan(
			scan,
			[]predicates.QueryPredicate{
				predicates.NewConstantPredicate(predicates.TriTrue),
			},
		)
		return mustConstruct(t, filter, err)
	}
	plain, err := plans.NewRecordQueryProjectionPlan(nil, filteredScan())
	plain = mustConstruct(t, plain, err)
	repeatedInner := filteredScan()
	inUnion, err := plans.NewRecordQueryInUnionPlan(
		repeatedInner,
		[]string{"in_value"},
		nil,
		false,
	)
	inUnion = mustConstruct(t, inUnion, err)
	inUnion.SetInSources([][]any{{int64(1), int64(2)}})
	repeated, err := plans.NewRecordQueryProjectionPlan(nil, inUnion)
	repeated = mustConstruct(t, repeated, err)

	if _, applicable := compareInOperator(plain); applicable {
		t.Fatal("root Projection unexpectedly activated the root-only IN penalty")
	}
	if _, applicable := compareInOperator(repeated); applicable {
		t.Fatal("root Projection unexpectedly activated the root-only IN penalty")
	}
	if plainResiduals, repeatedResiduals := countResidualPredicatesWithContext(plain, nil), countResidualPredicatesWithContext(repeated, nil); plainResiduals != 1 || repeatedResiduals != 1 {
		t.Fatalf(
			"residual-count precondition = (%d, %d), want (1, 1)",
			plainResiduals,
			repeatedResiduals,
		)
	}
	plainOps := findExpressionsByType(plain, nil, nil)
	repeatedOps := findExpressionsByType(repeated, nil, nil)
	if plainOps.scanCount != 1 || repeatedOps.scanCount != 1 {
		t.Fatalf("data-access precondition = (%d, %d), want (1, 1)", plainOps.scanCount, repeatedOps.scanCount)
	}

	plainCost := properties.EstimateCost(plain)
	repeatedCost := properties.EstimateCost(repeated)
	if !plainCost.Less(repeatedCost) {
		t.Fatalf(
			"one full scan cost %+v must beat two InUnion executions %+v",
			plainCost,
			repeatedCost,
		)
	}
	if !PlanningCostModelLess(plain, repeated) {
		t.Fatal("full comparator preferred the repeated full-scan InUnion")
	}
	if PlanningCostModelLess(repeated, plain) {
		t.Fatal("full comparator preferred both orientations")
	}
}
