package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

func TestImplementInUnionRule_FiresWithExplodeAndInner(t *testing.T) {
	t.Parallel()
	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	sw := &physicalScanWrapper{plan: scan}
	innerRef := expressions.NewFinalReference([]expressions.RelationalExpression{sw})
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
		if _, ok := r.(*physicalInUnionWrapper); ok {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("should yield physicalInUnionWrapper")
	}
}

func TestImplementInUnionRule_SkipsSingleQuantifier(t *testing.T) {
	t.Parallel()
	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	sw := &physicalScanWrapper{plan: scan}
	innerRef := expressions.NewFinalReference([]expressions.RelationalExpression{sw})
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
	sw := &physicalScanWrapper{plan: scan}
	innerRef := expressions.NewFinalReference([]expressions.RelationalExpression{sw})
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
	ordering := NewRichOrdering(
		map[values.Value][]OrderingBinding{
			a: {FixedBinding(eqRange)},
		},
		[]values.Value{a},
		false,
	)

	req := NewRequestedOrdering([]RequestedOrderingPart{
		{Value: a, SortOrder: RequestedSortOrderAscending},
	}, DistinctnessNotDistinct, false)

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
	if bindings[0].GetSortOrder() != ProvidedSortOrderAscending {
		t.Fatal("promoted binding should match requested ascending direction")
	}
}

func TestAdjustBindingsForInUnion_KeepsNonExplodeFixed(t *testing.T) {
	t.Parallel()
	explodeAliases := map[values.CorrelationIdentifier]struct{}{}

	a := &values.FieldValue{Field: "a", Typ: values.UnknownType}
	ordering := NewRichOrdering(
		map[values.Value][]OrderingBinding{
			a: {FixedBinding(nil)},
		},
		[]values.Value{a},
		false,
	)

	req := NewRequestedOrdering(nil, DistinctnessNotDistinct, false)
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
	result := adjustBindingsForInUnion(nil, nil, PreserveOrdering())
	if result != nil {
		t.Fatal("nil ordering should return nil")
	}
}
