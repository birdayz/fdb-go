package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func mustConstraintE2EConstruct[T any](value T, err error) T {
	if err != nil {
		panic("construct constraint-propagation fixture: " + err.Error())
	}
	return value
}

func constraintE2ERowType() values.Type {
	return values.NewRecordType("ConstraintE2ERow", false, []values.Field{
		{Name: "id", FieldType: values.NotNullLong, Ordinal: 0},
	})
}

func constraintE2EKey() values.Value {
	root := mustConstraintE2EConstruct(values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier("constraint_e2e_pk"), constraintE2ERowType()))
	return mustConstraintE2EConstruct(values.ResolveOrdinalSeedField(root, 0))
}

func constraintE2EScan(recordType string) (*plans.RecordQueryScanPlan, *expressions.Reference) {
	scan := mustConstraintE2EConstruct(plans.NewRecordQueryScanPlan(
		[]string{recordType}, constraintE2ERowType(), false)).
		WithKeyComponentTypes([]values.Type{values.NotNullLong}).
		WithPrimaryKey([]values.Value{constraintE2EKey()})
	ref := expressions.InitialOf(scan)
	pm := NewPlanPropertiesMap()
	pm.Add(scan)
	ref.SetPlanProperties(pm)
	return scan, ref
}

func TestConstraintPropagation_DistinctUnionPushesToLegs(t *testing.T) {
	t.Parallel()

	_, refA := constraintE2EScan("T")
	_, refB := constraintE2EScan("T")

	union := mustConstraintE2EConstruct(expressions.NewLogicalUnionExpression([]expressions.Quantifier{
		expressions.ForEachQuantifier(refA),
		expressions.ForEachQuantifier(refB),
	}))
	unionRef := expressions.InitialOf(union)

	distinct := mustConstraintE2EConstruct(expressions.NewLogicalDistinctExpression(
		expressions.ForEachQuantifier(unionRef)))
	rootRef := expressions.InitialOf(distinct)

	cm := NewConstraintMap()
	reqOrdering := properties.NewRequestedOrdering([]properties.RequestedOrderingPart{
		{
			Value:     constraintE2EKey(),
			SortOrder: properties.RequestedSortOrderAscending,
		},
	}, properties.DistinctnessNotDistinct, false)
	Set(cm, rootRef, RequestedOrderingConstraintKey, []*properties.RequestedOrdering{reqOrdering})

	for _, rule := range DefaultImplementationRules() {
		mustFireImplementationRule(t, rule, rootRef, cm)
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

	scan, _ := constraintE2EScan("T")
	sw := scan
	innerRef := expressions.InitialOf(sw)
	pm := NewPlanPropertiesMap()
	pm.Add(sw)
	innerRef.SetPlanProperties(pm)

	unique := mustConstraintE2EConstruct(expressions.NewLogicalUniqueExpression(
		expressions.ForEachQuantifier(innerRef)))
	rootRef := expressions.InitialOf(unique)

	results := mustFireImplementationRule(t, NewImplementUniqueRule(), rootRef)
	if len(results) == 0 {
		t.Fatal("rule should work without constraint map")
	}
}
