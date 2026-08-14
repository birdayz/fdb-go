package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
)

func requestedOrderingUniqueFixture() (
	expressions.Quantifier,
	*expressions.LogicalUniqueExpression,
	*expressions.Reference,
) {
	q := requestedOrderingQuantifier("T", "unique_input")
	unique := mustRequestedOrderingConstruct(expressions.NewLogicalUniqueExpression(q))
	return q, unique, expressions.InitialOf(unique)
}

func TestPushRequestedOrderingThroughUnique_PropagatesConstraint(t *testing.T) {
	t.Parallel()

	scanQ, unique, uniqueRef := requestedOrderingUniqueFixture()
	col1 := requestedOrderingField(scanQ, "COL1")

	cm := NewConstraintMap()
	ordering := properties.NewRequestedOrdering(
		[]properties.RequestedOrderingPart{
			{Value: col1, SortOrder: properties.RequestedSortOrderAscending},
		},
		properties.DistinctnessNotDistinct, false,
	)
	Set(cm, uniqueRef, RequestedOrderingConstraintKey, []*properties.RequestedOrdering{ordering})

	rule := NewPushRequestedOrderingThroughUniqueRule()
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), unique)
	if len(bindings) != 1 {
		t.Fatalf("matcher should match LogicalUniqueExpression, got %d bindings", len(bindings))
	}

	call := &ImplementationRuleCall{
		Bindings:       bindings[0],
		Reference:      uniqueRef,
		Constraints:    cm,
		constraintOnly: true,
	}
	mustRunRequestedOrderingRule(t, rule, call)

	innerRef := unique.GetInner().GetRangesOver()
	pushed, ok := Get(cm, innerRef, RequestedOrderingConstraintKey)
	if !ok {
		t.Fatal("constraint not pushed to child Reference")
	}
	if len(pushed) != 1 {
		t.Fatalf("expected 1 pushed ordering, got %d", len(pushed))
	}
	parts := pushed[0].GetParts()
	if len(parts) != 1 {
		t.Fatalf("expected 1 ordering part, got %d", len(parts))
	}
	assertRequestedOrderingField(t, parts[0].Value, col1)
}

func TestPushRequestedOrderingThroughUnique_NoConstraintDoesNotPush(t *testing.T) {
	t.Parallel()

	_, unique, uniqueRef := requestedOrderingUniqueFixture()

	cm := NewConstraintMap()

	rule := NewPushRequestedOrderingThroughUniqueRule()
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), unique)
	call := &ImplementationRuleCall{
		Bindings:       bindings[0],
		Reference:      uniqueRef,
		Constraints:    cm,
		constraintOnly: true,
	}
	mustRunRequestedOrderingRule(t, rule, call)

	innerRef := unique.GetInner().GetRangesOver()
	_, ok := Get(cm, innerRef, RequestedOrderingConstraintKey)
	if ok {
		t.Fatal("constraint should not be pushed when parent has no constraint")
	}
}

func TestPushRequestedOrderingThroughUnique_NotConstraintOnlyDoesNotPush(t *testing.T) {
	t.Parallel()

	scanQ, unique, uniqueRef := requestedOrderingUniqueFixture()
	col1 := requestedOrderingField(scanQ, "COL1")

	cm := NewConstraintMap()
	ordering := properties.NewRequestedOrdering(
		[]properties.RequestedOrderingPart{
			{Value: col1, SortOrder: properties.RequestedSortOrderAscending},
		},
		properties.DistinctnessNotDistinct, false,
	)
	Set(cm, uniqueRef, RequestedOrderingConstraintKey, []*properties.RequestedOrdering{ordering})

	rule := NewPushRequestedOrderingThroughUniqueRule()
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), unique)
	call := &ImplementationRuleCall{
		Bindings:       bindings[0],
		Reference:      uniqueRef,
		Constraints:    cm,
		constraintOnly: false,
	}
	mustRunRequestedOrderingRule(t, rule, call)

	innerRef := unique.GetInner().GetRangesOver()
	_, ok := Get(cm, innerRef, RequestedOrderingConstraintKey)
	if ok {
		t.Fatal("constraint should not be pushed during implementation pass")
	}
}

func TestPushRequestedOrderingThroughUnique_DescPreserved(t *testing.T) {
	t.Parallel()

	scanQ, unique, uniqueRef := requestedOrderingUniqueFixture()
	a := requestedOrderingField(scanQ, "A")

	cm := NewConstraintMap()
	ordering := properties.NewRequestedOrdering(
		[]properties.RequestedOrderingPart{
			{Value: a, SortOrder: properties.RequestedSortOrderDescending},
		},
		properties.DistinctnessNotDistinct, false,
	)
	Set(cm, uniqueRef, RequestedOrderingConstraintKey, []*properties.RequestedOrdering{ordering})

	rule := NewPushRequestedOrderingThroughUniqueRule()
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), unique)
	call := &ImplementationRuleCall{
		Bindings:       bindings[0],
		Reference:      uniqueRef,
		Constraints:    cm,
		constraintOnly: true,
	}
	mustRunRequestedOrderingRule(t, rule, call)

	innerRef := unique.GetInner().GetRangesOver()
	pushed, ok := Get(cm, innerRef, RequestedOrderingConstraintKey)
	if !ok {
		t.Fatal("constraint not pushed to child Reference")
	}
	if pushed[0].GetParts()[0].SortOrder != properties.RequestedSortOrderDescending {
		t.Fatal("DESC should be preserved through Unique")
	}
}

func TestPushRequestedOrderingThroughUnique_NoYield(t *testing.T) {
	t.Parallel()

	scanQ, unique, uniqueRef := requestedOrderingUniqueFixture()
	a := requestedOrderingField(scanQ, "A")

	cm := NewConstraintMap()
	ordering := properties.NewRequestedOrdering(
		[]properties.RequestedOrderingPart{
			{Value: a, SortOrder: properties.RequestedSortOrderAscending},
		},
		properties.DistinctnessNotDistinct, false,
	)
	Set(cm, uniqueRef, RequestedOrderingConstraintKey, []*properties.RequestedOrdering{ordering})

	rule := NewPushRequestedOrderingThroughUniqueRule()
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), unique)
	call := &ImplementationRuleCall{
		Bindings:       bindings[0],
		Reference:      uniqueRef,
		Constraints:    cm,
		constraintOnly: true,
	}
	mustRunRequestedOrderingRule(t, rule, call)

	if len(call.yielded) != 0 {
		t.Fatalf("constraint-push rule should not yield expressions, but yielded %d", len(call.yielded))
	}
}
