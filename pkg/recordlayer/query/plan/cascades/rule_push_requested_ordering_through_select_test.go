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
	rule.OnMatch(&ImplementationRuleCall{
		Bindings:       bindings[0],
		Reference:      ref,
		Constraints:    constraints,
		constraintOnly: true,
	})
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

	childRef := expressions.InitialOf(
		expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType),
	)
	childQ := expressions.ForEachQuantifier(childRef)
	childID := values.NewFieldValue(
		values.NewQuantifiedObjectValue(childQ.GetAlias()),
		"ID",
		values.NullableLong,
	)
	sel := expressions.NewSelectExpression(
		values.NewRecordConstructorValue(
			values.RecordConstructorField{Name: "OUT_ID", Value: childID},
		),
		[]expressions.Quantifier{childQ},
		nil,
	)
	selRef := expressions.InitialOf(sel)

	parentOrdering := properties.NewRequestedOrdering(
		[]properties.RequestedOrderingPart{{
			Value:     values.NewFlatFieldValue("OUT_ID", values.NullableLong),
			SortOrder: properties.RequestedSortOrderDescending,
		}},
		properties.DistinctnessNotDistinct,
		false,
	)
	constraints := NewConstraintMap()
	Set(
		constraints,
		selRef,
		RequestedOrderingConstraintKey,
		[]*properties.RequestedOrdering{parentOrdering},
	)

	fireConstraintOnlyRule(
		t,
		NewPushRequestedOrderingThroughSelectRule(),
		sel,
		selRef,
		constraints,
	)

	pushed := requirePushedOrdering(t, constraints, childRef)
	parts := pushed.GetParts()
	if len(parts) != 1 {
		t.Fatalf("pushed ordering has %d parts, want one", len(parts))
	}
	if !values.ValuesStructurallyEqual(parts[0].Value, childID) {
		t.Fatalf(
			"pushed ordering value = %s, want child value %s",
			values.ExplainValue(parts[0].Value),
			values.ExplainValue(childID),
		)
	}
	if parts[0].SortOrder != properties.RequestedSortOrderDescending {
		t.Fatalf("pushed sort order = %v, want descending", parts[0].SortOrder)
	}
}

func TestPushRequestedOrderingThroughSelectExistentialRule_PushesPreserve(t *testing.T) {
	t.Parallel()

	existsRef := expressions.InitialOf(
		expressions.NewFullUnorderedScanExpression([]string{"EXISTS_T"}, values.UnknownType),
	)
	existsQ := expressions.ExistentialQuantifier(existsRef)
	sel := expressions.NewSelectExpression(
		values.LiteralValue(true),
		[]expressions.Quantifier{existsQ},
		nil,
	)
	selRef := expressions.InitialOf(sel)
	constraints := NewConstraintMap()

	fireConstraintOnlyRule(
		t,
		NewPushRequestedOrderingThroughSelectExistentialRule(),
		sel,
		selRef,
		constraints,
	)

	if pushed := requirePushedOrdering(t, constraints, existsRef); !pushed.IsPreserve() {
		t.Fatalf("existential child ordering = %#v, want preserve", pushed.GetParts())
	}
}

func TestPushRequestedOrderingThroughInLikeSelectRule_PushesToInner(t *testing.T) {
	t.Parallel()

	explodeRef := expressions.InitialOf(
		expressions.NewExplodeExpression(values.LiteralValue([]any{int64(1), int64(2)})),
	)
	explodeQ := expressions.ForEachQuantifier(explodeRef)
	baseRef := expressions.InitialOf(
		expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType),
	)
	baseQ := expressions.ForEachQuantifier(baseRef)
	correlationPredicate := predicates.NewComparisonPredicate(
		values.NewQuantifiedObjectValue(explodeQ.GetAlias()),
		predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, int64(0)),
	)
	correlatedInner := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{correlationPredicate},
		baseQ,
	)
	innerRef := expressions.InitialOf(correlatedInner)
	innerQ := expressions.ForEachQuantifier(innerRef)
	if _, correlated := innerQ.GetCorrelatedTo()[explodeQ.GetAlias()]; !correlated {
		t.Fatal("positive IN-like fixture is not correlated to its explode quantifier")
	}
	sel := expressions.NewSelectExpression(
		values.NewQuantifiedObjectValue(innerQ.GetAlias()),
		[]expressions.Quantifier{explodeQ, innerQ},
		nil,
	)
	selRef := expressions.InitialOf(sel)

	parentOrdering := properties.NewRequestedOrdering(
		[]properties.RequestedOrderingPart{{
			Value:     values.NewFlatFieldValue("ID", values.NullableLong),
			SortOrder: properties.RequestedSortOrderAscending,
		}},
		properties.DistinctnessNotDistinct,
		false,
	)
	constraints := NewConstraintMap()
	Set(
		constraints,
		selRef,
		RequestedOrderingConstraintKey,
		[]*properties.RequestedOrdering{parentOrdering},
	)

	fireConstraintOnlyRule(
		t,
		NewPushRequestedOrderingThroughInLikeSelectRule(),
		sel,
		selRef,
		constraints,
	)

	pushed := requirePushedOrdering(t, constraints, innerRef)
	if pushed != parentOrdering {
		t.Fatal("IN-like SELECT did not push the parent ordering verbatim")
	}
}

func TestPushRequestedOrderingThroughRecursiveUnionRule_PushesPreserveToBothLegs(t *testing.T) {
	t.Parallel()

	initialRef := expressions.InitialOf(
		expressions.NewFullUnorderedScanExpression([]string{"SEED"}, values.UnknownType),
	)
	recursiveRef := expressions.InitialOf(
		expressions.NewFullUnorderedScanExpression([]string{"STEP"}, values.UnknownType),
	)
	union := expressions.NewRecursiveUnionExpression(
		expressions.ForEachQuantifier(initialRef),
		expressions.ForEachQuantifier(recursiveRef),
		values.NamedCorrelationIdentifier("recursive_scan"),
		values.NamedCorrelationIdentifier("recursive_insert"),
		expressions.TraversalPreorder,
	)
	unionRef := expressions.InitialOf(union)

	preserve := properties.PreserveOrdering()
	constraints := NewConstraintMap()
	Set(
		constraints,
		unionRef,
		RequestedOrderingConstraintKey,
		[]*properties.RequestedOrdering{preserve},
	)

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
