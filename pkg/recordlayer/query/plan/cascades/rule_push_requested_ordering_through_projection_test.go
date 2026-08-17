package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func runRequestedOrderingProjection(
	t *testing.T,
	projection *expressions.LogicalProjectionExpression,
	projectionRef *expressions.Reference,
	constraints *ConstraintMap,
	constraintOnly bool,
) *ImplementationRuleCall {
	t.Helper()
	rule := NewPushRequestedOrderingThroughProjectionRule()
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), projection)
	if len(bindings) != 1 {
		t.Fatalf("matcher bindings = %d, want one", len(bindings))
	}
	call := &ImplementationRuleCall{
		Bindings:       bindings[0],
		Reference:      projectionRef,
		Constraints:    constraints,
		constraintOnly: constraintOnly,
	}
	mustRunRequestedOrderingRule(t, rule, call)
	return call
}

func setProjectionRequestedOrdering(
	constraints *ConstraintMap,
	projectionRef *expressions.Reference,
	parts []properties.RequestedOrderingPart,
) {
	Set(constraints, projectionRef, RequestedOrderingConstraintKey, []*properties.RequestedOrdering{
		properties.NewRequestedOrdering(parts, properties.DistinctnessNotDistinct, false),
	})
}

func TestPushRequestedOrderingThroughProjection_PushesTranslatedOrdering(t *testing.T) {
	t.Parallel()
	_ = NewPushRequestedOrderingThroughProjectionRule() // direct behavioral-census anchor

	input := requestedOrderingQuantifier("T", "projection_input")
	a := requestedOrderingField(input, "A")
	b := requestedOrderingField(input, "B")
	projection := mustRequestedOrderingConstruct(expressions.NewLogicalProjectionExpressionWithAliases(
		[]values.Value{a, b}, []string{"col1", "col2"}, input))
	projectionRef := expressions.InitialOf(projection)
	constraints := NewConstraintMap()
	setProjectionRequestedOrdering(constraints, projectionRef, []properties.RequestedOrderingPart{{
		Value: requestedOrderingOutputField(
			projection.GetInner().GetAlias(), projection.GetResultValue(), 0),
		SortOrder: properties.RequestedSortOrderAscending,
	}})
	runRequestedOrderingProjection(t, projection, projectionRef, constraints, true)

	pushed, ok := Get(constraints, projection.GetInner().GetRangesOver(), RequestedOrderingConstraintKey)
	if !ok || len(pushed) != 1 || len(pushed[0].GetParts()) != 1 {
		t.Fatalf("pushed orderings = %v ok=%v, want one part", pushed, ok)
	}
	part := pushed[0].GetParts()[0]
	assertRequestedOrderingField(t, part.Value, a)
	if part.SortOrder != properties.RequestedSortOrderAscending {
		t.Fatalf("pushed direction = %v, want ASC", part.SortOrder)
	}
}

func TestPushRequestedOrderingThroughProjection_ComputedSlot(t *testing.T) {
	t.Parallel()

	input := requestedOrderingQuantifier("T", "projection_input")
	add := &values.ArithmeticValue{
		Left:  requestedOrderingField(input, "A"),
		Right: requestedOrderingField(input, "B"),
		Op:    values.OpAdd,
	}
	projection := mustRequestedOrderingConstruct(expressions.NewLogicalProjectionExpressionWithAliases(
		[]values.Value{add, requestedOrderingField(input, "C")},
		[]string{"total", "c"}, input))
	projectionRef := expressions.InitialOf(projection)
	constraints := NewConstraintMap()
	setProjectionRequestedOrdering(constraints, projectionRef, []properties.RequestedOrderingPart{{
		Value: requestedOrderingOutputField(
			projection.GetInner().GetAlias(), projection.GetResultValue(), 0),
		SortOrder: properties.RequestedSortOrderAscending,
	}})
	runRequestedOrderingProjection(t, projection, projectionRef, constraints, true)

	pushed, ok := Get(constraints, projection.GetInner().GetRangesOver(), RequestedOrderingConstraintKey)
	if !ok || len(pushed) != 1 || len(pushed[0].GetParts()) != 1 {
		t.Fatalf("pushed orderings = %v ok=%v, want one computed key", pushed, ok)
	}
	if !values.ValuesStructurallyEqual(pushed[0].GetParts()[0].Value, add) {
		t.Fatalf("pushed key = %s, want computed key %s",
			values.ExplainValue(pushed[0].GetParts()[0].Value), values.ExplainValue(add))
	}
}

func TestPushRequestedOrderingThroughProjection_NoMatchDoesNotPush(t *testing.T) {
	t.Parallel()

	input := requestedOrderingQuantifier("T", "projection_input")
	projection := mustRequestedOrderingConstruct(expressions.NewLogicalProjectionExpressionWithAliases(
		[]values.Value{requestedOrderingField(input, "A")}, []string{"col1"}, input))
	projectionRef := expressions.InitialOf(projection)
	foreignOutput := values.NewRecordType("projection_foreign_output", false, []values.Field{
		{Name: "NONEXISTENT", FieldType: values.NullableLong},
	})
	wrongSlot := requestedOrderingFieldForType(
		projection.GetInner().GetAlias(), foreignOutput, "NONEXISTENT")
	constraints := NewConstraintMap()
	setProjectionRequestedOrdering(constraints, projectionRef, []properties.RequestedOrderingPart{{
		Value: wrongSlot, SortOrder: properties.RequestedSortOrderAscending,
	}})
	runRequestedOrderingProjection(t, projection, projectionRef, constraints, true)

	if _, ok := Get(constraints, projection.GetInner().GetRangesOver(), RequestedOrderingConstraintKey); ok {
		t.Fatal("ordering from a different exact output layout must fail closed")
	}
}

func TestPushRequestedOrderingThroughProjection_DescPreserved(t *testing.T) {
	t.Parallel()

	input := requestedOrderingQuantifier("T", "projection_input")
	a := requestedOrderingField(input, "A")
	projection := mustRequestedOrderingConstruct(expressions.NewLogicalProjectionExpressionWithAliases(
		[]values.Value{a}, []string{"a"}, input))
	projectionRef := expressions.InitialOf(projection)
	constraints := NewConstraintMap()
	setProjectionRequestedOrdering(constraints, projectionRef, []properties.RequestedOrderingPart{{
		Value: requestedOrderingOutputField(
			projection.GetInner().GetAlias(), projection.GetResultValue(), 0),
		SortOrder: properties.RequestedSortOrderDescending,
	}})
	runRequestedOrderingProjection(t, projection, projectionRef, constraints, true)

	pushed, ok := Get(constraints, projection.GetInner().GetRangesOver(), RequestedOrderingConstraintKey)
	if !ok || len(pushed) != 1 || len(pushed[0].GetParts()) != 1 {
		t.Fatalf("pushed orderings = %v ok=%v, want one part", pushed, ok)
	}
	part := pushed[0].GetParts()[0]
	assertRequestedOrderingField(t, part.Value, a)
	if part.SortOrder != properties.RequestedSortOrderDescending {
		t.Fatalf("pushed direction = %v, want DESC", part.SortOrder)
	}
}

func TestPushRequestedOrderingThroughProjection_MultipleSortKeys(t *testing.T) {
	t.Parallel()

	input := requestedOrderingQuantifier("T", "projection_input")
	x := requestedOrderingField(input, "X")
	y := requestedOrderingField(input, "Y")
	projection := mustRequestedOrderingConstruct(expressions.NewLogicalProjectionExpressionWithAliases(
		[]values.Value{x, y, requestedOrderingField(input, "Z")},
		[]string{"a", "b", "c"}, input))
	projectionRef := expressions.InitialOf(projection)
	constraints := NewConstraintMap()
	setProjectionRequestedOrdering(constraints, projectionRef, []properties.RequestedOrderingPart{
		{
			Value: requestedOrderingOutputField(
				projection.GetInner().GetAlias(), projection.GetResultValue(), 0),
			SortOrder: properties.RequestedSortOrderAscending,
		},
		{
			Value: requestedOrderingOutputField(
				projection.GetInner().GetAlias(), projection.GetResultValue(), 1),
			SortOrder: properties.RequestedSortOrderDescending,
		},
	})
	runRequestedOrderingProjection(t, projection, projectionRef, constraints, true)

	pushed, ok := Get(constraints, projection.GetInner().GetRangesOver(), RequestedOrderingConstraintKey)
	if !ok || len(pushed) != 1 || len(pushed[0].GetParts()) != 2 {
		t.Fatalf("pushed orderings = %v ok=%v, want two parts", pushed, ok)
	}
	parts := pushed[0].GetParts()
	assertRequestedOrderingField(t, parts[0].Value, x)
	assertRequestedOrderingField(t, parts[1].Value, y)
	if parts[0].SortOrder != properties.RequestedSortOrderAscending ||
		parts[1].SortOrder != properties.RequestedSortOrderDescending {
		t.Fatalf("pushed directions = [%v, %v], want [ASC, DESC]",
			parts[0].SortOrder, parts[1].SortOrder)
	}
}

func TestPushRequestedOrderingThroughProjection_NotConstraintOnlyDoesNotPush(t *testing.T) {
	t.Parallel()

	input := requestedOrderingQuantifier("T", "projection_input")
	projection := mustRequestedOrderingConstruct(expressions.NewLogicalProjectionExpressionWithAliases(
		[]values.Value{requestedOrderingField(input, "A")}, []string{"a"}, input))
	projectionRef := expressions.InitialOf(projection)
	constraints := NewConstraintMap()
	setProjectionRequestedOrdering(constraints, projectionRef, []properties.RequestedOrderingPart{{
		Value: requestedOrderingOutputField(
			projection.GetInner().GetAlias(), projection.GetResultValue(), 0),
		SortOrder: properties.RequestedSortOrderAscending,
	}})
	runRequestedOrderingProjection(t, projection, projectionRef, constraints, false)
	if _, ok := Get(constraints, projection.GetInner().GetRangesOver(), RequestedOrderingConstraintKey); ok {
		t.Fatal("constraint should not be pushed during the implementation pass")
	}
}

func TestPushRequestedOrderingThroughProjection_NoOrderingConstraint(t *testing.T) {
	t.Parallel()

	input := requestedOrderingQuantifier("T", "projection_input")
	projection := mustRequestedOrderingConstruct(expressions.NewLogicalProjectionExpressionWithAliases(
		[]values.Value{requestedOrderingField(input, "A")}, []string{"a"}, input))
	projectionRef := expressions.InitialOf(projection)
	constraints := NewConstraintMap()
	runRequestedOrderingProjection(t, projection, projectionRef, constraints, true)
	if _, ok := Get(constraints, projection.GetInner().GetRangesOver(), RequestedOrderingConstraintKey); ok {
		t.Fatal("constraint should not be pushed when none exists above the projection")
	}
}

func TestPushRequestedOrderingThroughProjection_NoYield(t *testing.T) {
	t.Parallel()

	input := requestedOrderingQuantifier("T", "projection_input")
	projection := mustRequestedOrderingConstruct(expressions.NewLogicalProjectionExpressionWithAliases(
		[]values.Value{requestedOrderingField(input, "A")}, []string{"a"}, input))
	projectionRef := expressions.InitialOf(projection)
	constraints := NewConstraintMap()
	setProjectionRequestedOrdering(constraints, projectionRef, []properties.RequestedOrderingPart{{
		Value: requestedOrderingOutputField(
			projection.GetInner().GetAlias(), projection.GetResultValue(), 0),
		SortOrder: properties.RequestedSortOrderAscending,
	}})
	call := runRequestedOrderingProjection(t, projection, projectionRef, constraints, true)
	if len(call.yielded) != 0 {
		t.Fatalf("constraint-push rule yielded %d expressions, want none", len(call.yielded))
	}
}

// A resolved ordinal alone is not sufficient: the request must be rooted at
// the exact projection output alias. This catches same-shaped sibling outputs.
func TestPushRequestedOrderingThroughProjection_LazyRequestDoesNotPush(t *testing.T) {
	t.Parallel()

	input := requestedOrderingQuantifier("T", "projection_input")
	projection := mustRequestedOrderingConstruct(expressions.NewLogicalProjectionExpressionWithAliases(
		[]values.Value{requestedOrderingField(input, "A")}, []string{"col1"}, input))
	projectionRef := expressions.InitialOf(projection)
	foreignAliasRequest := requestedOrderingOutputField(
		values.NamedCorrelationIdentifier("foreign_projection_output"),
		projection.GetResultValue(),
		0,
	)
	constraints := NewConstraintMap()
	setProjectionRequestedOrdering(constraints, projectionRef, []properties.RequestedOrderingPart{{
		Value: foreignAliasRequest, SortOrder: properties.RequestedSortOrderAscending,
	}})
	runRequestedOrderingProjection(t, projection, projectionRef, constraints, true)
	if _, ok := Get(constraints, projection.GetInner().GetRangesOver(), RequestedOrderingConstraintKey); ok {
		t.Fatal("ordering rooted at a foreign projection alias must fail closed")
	}
}
