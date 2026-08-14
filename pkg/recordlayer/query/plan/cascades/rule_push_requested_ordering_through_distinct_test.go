package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
)

func requestedOrderingDistinctFixture() (
	expressions.Quantifier,
	*expressions.LogicalDistinctExpression,
	*expressions.Reference,
) {
	q := requestedOrderingQuantifier("T", "distinct_input")
	distinct := mustRequestedOrderingConstruct(expressions.NewLogicalDistinctExpression(q))
	return q, distinct, expressions.InitialOf(distinct)
}

func TestPushRequestedOrderingThroughDistinct_PropagatesConstraint(t *testing.T) {
	t.Parallel()

	// Build: Distinct(Scan)
	scanQ, distinct, distinctRef := requestedOrderingDistinctFixture()
	col1 := requestedOrderingField(scanQ, "COL1")

	// Set a requested ordering constraint on the Distinct's Reference
	// (as if pushed by a parent Sort rule).
	cm := NewConstraintMap()
	ordering := properties.NewRequestedOrdering(
		[]properties.RequestedOrderingPart{
			{Value: col1, SortOrder: properties.RequestedSortOrderAscending},
		},
		properties.DistinctnessNotDistinct, false,
	)
	Set(cm, distinctRef, RequestedOrderingConstraintKey, []*properties.RequestedOrdering{ordering})

	// Fire the rule in constraintOnly mode.
	rule := NewPushRequestedOrderingThroughDistinctRule()
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), distinct)
	if len(bindings) != 1 {
		t.Fatalf("matcher should match LogicalDistinctExpression, got %d bindings", len(bindings))
	}

	call := &ImplementationRuleCall{
		Bindings:       bindings[0],
		Reference:      distinctRef,
		Constraints:    cm,
		constraintOnly: true,
	}
	mustRunRequestedOrderingRule(t, rule, call)

	// The constraint should now be pushed to the inner (scan) Reference.
	innerRef := distinct.GetInner().GetRangesOver()
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
	if parts[0].SortOrder != properties.RequestedSortOrderAscending {
		t.Fatal("expected ASC sort order")
	}
}

func TestPushRequestedOrderingThroughDistinct_NoConstraintDoesNotPush(t *testing.T) {
	t.Parallel()

	_, distinct, distinctRef := requestedOrderingDistinctFixture()

	cm := NewConstraintMap()

	rule := NewPushRequestedOrderingThroughDistinctRule()
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), distinct)
	call := &ImplementationRuleCall{
		Bindings:       bindings[0],
		Reference:      distinctRef,
		Constraints:    cm,
		constraintOnly: true,
	}
	mustRunRequestedOrderingRule(t, rule, call)

	// No constraint set on parent → nothing pushed to child.
	innerRef := distinct.GetInner().GetRangesOver()
	_, ok := Get(cm, innerRef, RequestedOrderingConstraintKey)
	if ok {
		t.Fatal("constraint should not be pushed when parent has no constraint")
	}
}

func TestPushRequestedOrderingThroughDistinct_NotConstraintOnlyDoesNotPush(t *testing.T) {
	t.Parallel()

	scanQ, distinct, distinctRef := requestedOrderingDistinctFixture()
	col1 := requestedOrderingField(scanQ, "COL1")

	cm := NewConstraintMap()
	ordering := properties.NewRequestedOrdering(
		[]properties.RequestedOrderingPart{
			{Value: col1, SortOrder: properties.RequestedSortOrderAscending},
		},
		properties.DistinctnessNotDistinct, false,
	)
	Set(cm, distinctRef, RequestedOrderingConstraintKey, []*properties.RequestedOrdering{ordering})

	rule := NewPushRequestedOrderingThroughDistinctRule()
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), distinct)
	call := &ImplementationRuleCall{
		Bindings:       bindings[0],
		Reference:      distinctRef,
		Constraints:    cm,
		constraintOnly: false, // bottom-up implementation pass
	}
	mustRunRequestedOrderingRule(t, rule, call)

	// constraintOnly=false → rule is a no-op, nothing pushed.
	innerRef := distinct.GetInner().GetRangesOver()
	_, ok := Get(cm, innerRef, RequestedOrderingConstraintKey)
	if ok {
		t.Fatal("constraint should not be pushed during implementation pass")
	}
}

func TestPushRequestedOrderingThroughDistinct_MultipleSortKeys(t *testing.T) {
	t.Parallel()

	scanQ, distinct, distinctRef := requestedOrderingDistinctFixture()
	a := requestedOrderingField(scanQ, "A")
	b := requestedOrderingField(scanQ, "B")

	cm := NewConstraintMap()
	ordering := properties.NewRequestedOrdering(
		[]properties.RequestedOrderingPart{
			{Value: a, SortOrder: properties.RequestedSortOrderAscending},
			{Value: b, SortOrder: properties.RequestedSortOrderDescending},
		},
		properties.DistinctnessNotDistinct, false,
	)
	Set(cm, distinctRef, RequestedOrderingConstraintKey, []*properties.RequestedOrdering{ordering})

	rule := NewPushRequestedOrderingThroughDistinctRule()
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), distinct)
	call := &ImplementationRuleCall{
		Bindings:       bindings[0],
		Reference:      distinctRef,
		Constraints:    cm,
		constraintOnly: true,
	}
	mustRunRequestedOrderingRule(t, rule, call)

	innerRef := distinct.GetInner().GetRangesOver()
	pushed, ok := Get(cm, innerRef, RequestedOrderingConstraintKey)
	if !ok {
		t.Fatal("constraint not pushed to child Reference")
	}
	parts := pushed[0].GetParts()
	if len(parts) != 2 {
		t.Fatalf("expected 2 ordering parts, got %d", len(parts))
	}
	assertRequestedOrderingField(t, parts[0].Value, a)
	assertRequestedOrderingField(t, parts[1].Value, b)
	if parts[0].SortOrder != properties.RequestedSortOrderAscending ||
		parts[1].SortOrder != properties.RequestedSortOrderDescending {
		t.Fatalf("sort directions = [%v, %v], want [ASC, DESC]",
			parts[0].SortOrder, parts[1].SortOrder)
	}
}

func TestPushRequestedOrderingThroughDistinct_DescPreserved(t *testing.T) {
	t.Parallel()

	scanQ, distinct, distinctRef := requestedOrderingDistinctFixture()
	a := requestedOrderingField(scanQ, "A")

	cm := NewConstraintMap()
	ordering := properties.NewRequestedOrdering(
		[]properties.RequestedOrderingPart{
			{Value: a, SortOrder: properties.RequestedSortOrderDescending},
		},
		properties.DistinctnessNotDistinct, false,
	)
	Set(cm, distinctRef, RequestedOrderingConstraintKey, []*properties.RequestedOrdering{ordering})

	rule := NewPushRequestedOrderingThroughDistinctRule()
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), distinct)
	call := &ImplementationRuleCall{
		Bindings:       bindings[0],
		Reference:      distinctRef,
		Constraints:    cm,
		constraintOnly: true,
	}
	mustRunRequestedOrderingRule(t, rule, call)

	innerRef := distinct.GetInner().GetRangesOver()
	pushed, ok := Get(cm, innerRef, RequestedOrderingConstraintKey)
	if !ok {
		t.Fatal("constraint not pushed to child Reference")
	}
	if pushed[0].GetParts()[0].SortOrder != properties.RequestedSortOrderDescending {
		t.Fatal("DESC should be preserved through Distinct")
	}
}

func TestPushRequestedOrderingThroughDistinct_NoYield(t *testing.T) {
	t.Parallel()

	// The constraint-push rule should never yield expressions.
	scanQ, distinct, distinctRef := requestedOrderingDistinctFixture()
	a := requestedOrderingField(scanQ, "A")

	cm := NewConstraintMap()
	ordering := properties.NewRequestedOrdering(
		[]properties.RequestedOrderingPart{
			{Value: a, SortOrder: properties.RequestedSortOrderAscending},
		},
		properties.DistinctnessNotDistinct, false,
	)
	Set(cm, distinctRef, RequestedOrderingConstraintKey, []*properties.RequestedOrdering{ordering})

	rule := NewPushRequestedOrderingThroughDistinctRule()
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), distinct)
	call := &ImplementationRuleCall{
		Bindings:       bindings[0],
		Reference:      distinctRef,
		Constraints:    cm,
		constraintOnly: true,
	}
	mustRunRequestedOrderingRule(t, rule, call)

	if len(call.yielded) != 0 {
		t.Fatalf("constraint-push rule should not yield expressions, but yielded %d", len(call.yielded))
	}
}
