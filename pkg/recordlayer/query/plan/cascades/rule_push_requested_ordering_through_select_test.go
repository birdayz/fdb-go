package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func fireConstraintOnlyRule(
	t *testing.T,
	rule ImplementationRule,
	subject expressions.RelationalExpression,
	ref *expressions.Reference,
	constraints *ConstraintMap,
) {
	t.Helper()
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), subject)
	if len(bindings) != 1 {
		t.Fatalf("%T matcher produced %d bindings, want one", rule, len(bindings))
	}
	call := &ImplementationRuleCall{
		Bindings:       bindings[0],
		Reference:      ref,
		Constraints:    constraints,
		constraintOnly: true,
	}
	mustRunRequestedOrderingRule(t, rule, call)
}

func requirePushedOrdering(
	t *testing.T,
	constraints *ConstraintMap,
	ref *expressions.Reference,
) *properties.RequestedOrdering {
	t.Helper()
	pushed, ok := Get(constraints, ref, RequestedOrderingConstraintKey)
	if !ok {
		t.Fatal("requested ordering was not pushed to the child reference")
	}
	if len(pushed) != 1 {
		t.Fatalf("pushed %d requested orderings, want one", len(pushed))
	}
	return pushed[0]
}

func TestPushRequestedOrderingThroughSelectRule_TranslatesToChild(t *testing.T) {
	t.Parallel()

	child := requestedOrderingQuantifier("T", "select_child")
	childID := requestedOrderingField(child, "ID")
	result := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "OUT_ID", Value: childID})
	selectExpr := mustRequestedOrderingConstruct(expressions.NewSelectExpression(
		result, []expressions.Quantifier{child}, nil))
	selectRef := expressions.InitialOf(selectExpr)
	parentOrdering := properties.NewRequestedOrdering(
		[]properties.RequestedOrderingPart{{
			Value:     requestedOrderingCurrentOutputField(selectExpr.GetResultValue(), 0),
			SortOrder: properties.RequestedSortOrderDescending,
		}},
		properties.DistinctnessNotDistinct,
		false,
	)
	constraints := NewConstraintMap()
	Set(constraints, selectRef, RequestedOrderingConstraintKey,
		[]*properties.RequestedOrdering{parentOrdering})

	fireConstraintOnlyRule(
		t, NewPushRequestedOrderingThroughSelectRule(), selectExpr, selectRef, constraints)

	pushed := requirePushedOrdering(t, constraints, child.GetRangesOver())
	parts := pushed.GetParts()
	if len(parts) != 1 {
		t.Fatalf("pushed ordering has %d parts, want one", len(parts))
	}
	assertRequestedOrderingField(t, parts[0].Value, requestedOrderingCurrentField(child, "ID"))
	if parts[0].SortOrder != properties.RequestedSortOrderDescending {
		t.Fatalf("pushed sort order = %v, want descending", parts[0].SortOrder)
	}
}

func TestPushRequestedOrderingThroughSelectExistentialRule_PushesPreserve(t *testing.T) {
	t.Parallel()

	existsScan := requestedOrderingScan("EXISTS_T")
	existsRef := expressions.InitialOf(existsScan)
	existsQ := expressions.ExistentialQuantifier(existsRef)
	selectExpr := mustRequestedOrderingConstruct(expressions.NewSelectExpression(
		&values.ConstantValue{Value: true, Typ: values.NotNullBoolean},
		[]expressions.Quantifier{existsQ},
		nil,
	))
	selectRef := expressions.InitialOf(selectExpr)
	constraints := NewConstraintMap()

	fireConstraintOnlyRule(
		t,
		NewPushRequestedOrderingThroughSelectExistentialRule(),
		selectExpr,
		selectRef,
		constraints,
	)
	if pushed := requirePushedOrdering(t, constraints, existsRef); !pushed.IsPreserve() {
		t.Fatalf("existential child ordering = %#v, want preserve", pushed.GetParts())
	}
}

func TestPushRequestedOrderingThroughInLikeSelectRule_PushesToInner(t *testing.T) {
	t.Parallel()

	explode := mustRequestedOrderingConstruct(expressions.NewExplodeExpression(
		&values.ConstantValue{
			Value: []any{int64(1), int64(2)},
			Typ:   values.NewArrayType(false, values.NotNullLong),
		},
	))
	explodeRef := expressions.InitialOf(explode)
	explodeQ := expressions.NamedForEachQuantifier(
		values.NamedCorrelationIdentifier("select_explode"), explodeRef)
	explodeValue := mustRequestedOrderingConstruct(explodeQ.RequireFlowedObjectValue())
	baseQ := requestedOrderingQuantifier("T", "select_base")
	correlationPredicate := predicates.NewComparisonPredicate(
		explodeValue,
		predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, int64(0)),
	)
	correlatedInner := mustRequestedOrderingConstruct(expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{correlationPredicate}, baseQ))
	innerRef := expressions.InitialOf(correlatedInner)
	innerQ := expressions.NamedForEachQuantifier(
		values.NamedCorrelationIdentifier("select_inner"), innerRef)
	if _, correlated := innerQ.GetCorrelatedTo()[explodeQ.GetAlias()]; !correlated {
		t.Fatal("positive IN-like fixture is not correlated to its explode quantifier")
	}
	innerResult := mustRequestedOrderingConstruct(innerQ.RequireFlowedObjectValue())
	selectExpr := mustRequestedOrderingConstruct(expressions.NewSelectExpression(
		innerResult, []expressions.Quantifier{explodeQ, innerQ}, nil))
	selectRef := expressions.InitialOf(selectExpr)
	parentOrdering := properties.NewRequestedOrdering(
		[]properties.RequestedOrderingPart{{
			Value:     requestedOrderingField(innerQ, "ID"),
			SortOrder: properties.RequestedSortOrderAscending,
		}},
		properties.DistinctnessNotDistinct,
		false,
	)
	constraints := NewConstraintMap()
	Set(constraints, selectRef, RequestedOrderingConstraintKey,
		[]*properties.RequestedOrdering{parentOrdering})

	fireConstraintOnlyRule(
		t,
		NewPushRequestedOrderingThroughInLikeSelectRule(),
		selectExpr,
		selectRef,
		constraints,
	)
	pushed := requirePushedOrdering(t, constraints, innerRef)
	if pushed != parentOrdering {
		t.Fatal("IN-like SELECT did not push the exact parent ordering object verbatim")
	}
}

func TestPushRequestedOrderingThroughRecursiveUnionRule_PushesPreserveToBothLegs(t *testing.T) {
	t.Parallel()

	initialRef := expressions.InitialOf(requestedOrderingScan("SEED"))
	recursiveRef := expressions.InitialOf(requestedOrderingScan("STEP"))
	union := mustRequestedOrderingConstruct(expressions.NewRecursiveUnionExpression(
		expressions.NamedForEachQuantifier(
			values.NamedCorrelationIdentifier("recursive_initial"), initialRef),
		expressions.NamedForEachQuantifier(
			values.NamedCorrelationIdentifier("recursive_step"), recursiveRef),
		values.NamedCorrelationIdentifier("recursive_scan"),
		values.NamedCorrelationIdentifier("recursive_insert"),
		expressions.TraversalPreorder,
	))
	unionRef := expressions.InitialOf(union)
	preserve := properties.PreserveOrdering()
	constraints := NewConstraintMap()
	Set(constraints, unionRef, RequestedOrderingConstraintKey,
		[]*properties.RequestedOrdering{preserve})

	fireConstraintOnlyRule(
		t,
		NewPushRequestedOrderingThroughRecursiveUnionRule(),
		union,
		unionRef,
		constraints,
	)
	for name, ref := range map[string]*expressions.Reference{
		"initial":   initialRef,
		"recursive": recursiveRef,
	} {
		if pushed := requirePushedOrdering(t, constraints, ref); !pushed.IsPreserve() {
			t.Fatalf("%s leg ordering = %#v, want preserve", name, pushed.GetParts())
		}
	}
}
