package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

func TestConstraintPropagation_DistinctUnionPushesToLegs(t *testing.T) {
	t.Parallel()

	_, refA := makeScanWithPK("T", "id")
	_, refB := makeScanWithPK("T", "id")

	union := expressions.NewLogicalUnionExpression([]expressions.Quantifier{
		expressions.ForEachQuantifier(refA),
		expressions.ForEachQuantifier(refB),
	})
	unionRef := expressions.InitialOf(union)

	distinct := expressions.NewLogicalDistinctExpression(
		expressions.ForEachQuantifier(unionRef))
	rootRef := expressions.InitialOf(distinct)

	cm := NewConstraintMap()
	reqOrdering := NewRequestedOrdering([]RequestedOrderingPart{
		{
			Value:     &values.FieldValue{Field: "id", Typ: values.UnknownType},
			SortOrder: RequestedSortOrderAscending,
		},
	}, DistinctnessNotDistinct, false)
	Set(cm, rootRef, RequestedOrderingConstraintKey, []*RequestedOrdering{reqOrdering})

	for _, rule := range DefaultImplementationRules() {
		FireImplementationRule(rule, rootRef, cm)
	}

	gotA, okA := Get(cm, refA, RequestedOrderingConstraintKey)
	gotB, okB := Get(cm, refB, RequestedOrderingConstraintKey)

	if !okA || !okB {
		t.Fatalf("constraints should be pushed to both union legs: A=%v B=%v", okA, okB)
	}
	if len(gotA) == 0 || len(gotB) == 0 {
		t.Fatal("pushed constraints should be non-empty")
	}
}

func TestConstraintPropagation_NilConstraintMap(t *testing.T) {
	t.Parallel()

	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	sw := &physicalScanWrapper{plan: scan}
	innerRef := expressions.InitialOf(sw)
	pm := NewPlanPropertiesMap()
	pm.Add(sw)
	innerRef.SetPlanProperties(pm)

	unique := expressions.NewLogicalUniqueExpression(
		expressions.ForEachQuantifier(innerRef))
	rootRef := expressions.InitialOf(unique)

	results := FireImplementationRule(NewImplementUniqueRule(), rootRef)
	if len(results) == 0 {
		t.Fatal("rule should work without constraint map")
	}
}
