package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func TestImplementInUnionRule_FiresWithExplodeAndInner(t *testing.T) {
	t.Parallel()
	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	sw := scan
	innerRef := expressions.InitialOf(sw)
	pm := NewPlanPropertiesMap()
	pm.Add(sw)
	innerRef.SetPlanProperties(pm)

	innerQ := expressions.ForEachQuantifier(innerRef)

	explodeRef := expressions.InitialOf(
		expressions.NewExplodeExpression(&values.ConstantValue{Value: []any{1, 2, 3}}),
	)
	explodeQ := expressions.ForEachQuantifier(explodeRef)

	sel := expressions.NewSelectExpression(
		values.NewQuantifiedObjectValue(innerQ.GetAlias()),
		[]expressions.Quantifier{explodeQ, innerQ},
		nil,
	)

	outerRef := expressions.InitialOf(sel)
	results := FireImplementationRule(NewImplementInUnionRule(), outerRef)
	if len(results) == 0 {
		t.Fatal("should fire with explode + inner quantifier")
	}

	found := false
	for _, r := range results {
		if _, ok := r.(*plans.RecordQueryInUnionPlan); ok {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("should yield *RecordQueryInUnionPlan")
	}
}

func TestImplementInUnionRule_StrictSingleFailsClosed(t *testing.T) {
	t.Parallel()

	innerRef := expressions.InitialOf(
		expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType))
	innerAlias := values.NamedCorrelationIdentifier("STRICT")
	innerQ := expressions.NamedForEachStrictSingleQuantifier(innerAlias, innerRef)
	explodeQ := expressions.ForEachQuantifier(expressions.InitialOf(
		expressions.NewExplodeExpression(values.LiteralValue([]any{int64(1), int64(2)}))))
	sel := expressions.NewSelectExpression(
		innerQ.GetFlowedObjectValue(),
		[]expressions.Quantifier{explodeQ, innerQ},
		nil,
	)

	results := FireImplementationRule(
		NewImplementInUnionRule(), expressions.InitialOf(sel))
	if len(results) != 0 {
		t.Fatalf("strict-single IN shape yielded %d InUnion implementation(s), want zero", len(results))
	}
}

func TestImplementInUnionRule_SkipsSingleQuantifier(t *testing.T) {
	t.Parallel()
	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	sw := scan
	innerRef := expressions.InitialOf(sw)
	pm := NewPlanPropertiesMap()
	pm.Add(sw)
	innerRef.SetPlanProperties(pm)

	q := expressions.ForEachQuantifier(innerRef)
	sel := expressions.NewSelectExpression(
		values.NewQuantifiedObjectValue(q.GetAlias()),
		[]expressions.Quantifier{q},
		nil,
	)

	outerRef := expressions.InitialOf(sel)
	results := FireImplementationRule(NewImplementInUnionRule(), outerRef)
	if len(results) != 0 {
		t.Fatalf("should not fire with single quantifier, got %d", len(results))
	}
}

func TestImplementInUnionRule_SkipsWithPredicates(t *testing.T) {
	t.Parallel()
	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	sw := scan
	innerRef := expressions.InitialOf(sw)
	pm := NewPlanPropertiesMap()
	pm.Add(sw)
	innerRef.SetPlanProperties(pm)

	q := expressions.ForEachQuantifier(innerRef)
	explodeRef := expressions.InitialOf(
		expressions.NewExplodeExpression(&values.ConstantValue{Value: []any{1}}),
	)
	eq := expressions.ForEachQuantifier(explodeRef)

	pred := []predicates.QueryPredicate{predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "x", Typ: values.UnknownType},
		predicates.NewLiteralComparison(predicates.ComparisonEquals, 42),
	)}
	sel := expressions.NewSelectExpression(
		values.NewQuantifiedObjectValue(q.GetAlias()),
		[]expressions.Quantifier{eq, q},
		pred,
	)

	outerRef := expressions.InitialOf(sel)
	results := FireImplementationRule(NewImplementInUnionRule(), outerRef)
	if len(results) != 0 {
		t.Fatalf("should not fire with predicates, got %d", len(results))
	}
}

func TestAdjustBindingsForInUnion_PromotesExplodeAlias(t *testing.T) {
	t.Parallel()
	explodeAlias := values.NamedCorrelationIdentifier("explode_1")
	explodeAliases := map[values.CorrelationIdentifier]struct{}{explodeAlias: {}}

	eqComp := predicates.Comparison{
		Type:    predicates.ComparisonEquals,
		Operand: &values.QuantifiedObjectValue{Correlation: explodeAlias},
	}
	result := predicates.EmptyComparisonRange().Merge(&eqComp)
	if !result.Ok {
		t.Fatal("merge should succeed")
	}
	eqRange := result.Range

	a := &values.FieldValue{Field: "a", Typ: values.UnknownType}
	ordering := properties.NewRichOrdering(
		map[values.Value][]properties.OrderingBinding{
			a: {properties.FixedBinding(eqRange)},
		},
		[]values.Value{a},
		false,
	)

	req := properties.NewRequestedOrdering([]properties.RequestedOrderingPart{
		{Value: a, SortOrder: properties.RequestedSortOrderAscending},
	}, properties.DistinctnessNotDistinct, false)

	adjusted := adjustBindingsForInUnion(ordering, explodeAliases, req)
	if adjusted == nil {
		t.Fatal("adjustment should succeed")
	}

	bindings := adjusted.GetBindingMap()[a]
	if len(bindings) != 1 {
		t.Fatalf("expected 1 binding, got %d", len(bindings))
	}
	if !bindings[0].IsSorted() {
		t.Fatal("fixed binding referencing explode alias should be promoted to sorted")
	}
	if bindings[0].GetSortOrder() != properties.ProvidedSortOrderAscending {
		t.Fatal("promoted binding should match requested ascending direction")
	}
}

func TestAdjustBindingsForInUnion_KeepsNonExplodeFixed(t *testing.T) {
	t.Parallel()
	explodeAliases := map[values.CorrelationIdentifier]struct{}{}

	a := &values.FieldValue{Field: "a", Typ: values.UnknownType}
	ordering := properties.NewRichOrdering(
		map[values.Value][]properties.OrderingBinding{
			a: {properties.FixedBinding(nil)},
		},
		[]values.Value{a},
		false,
	)

	req := properties.NewRequestedOrdering(nil, properties.DistinctnessNotDistinct, false)
	adjusted := adjustBindingsForInUnion(ordering, explodeAliases, req)
	if adjusted == nil {
		t.Fatal("adjustment should succeed")
	}

	bindings := adjusted.GetBindingMap()[a]
	if len(bindings) != 1 || !bindings[0].IsFixed() {
		t.Fatal("non-explode fixed binding should remain fixed")
	}
}

func TestAdjustBindingsForInUnion_NilOrdering(t *testing.T) {
	t.Parallel()
	result := adjustBindingsForInUnion(nil, nil, properties.PreserveOrdering())
	if result != nil {
		t.Fatal("nil ordering should return nil")
	}
}
