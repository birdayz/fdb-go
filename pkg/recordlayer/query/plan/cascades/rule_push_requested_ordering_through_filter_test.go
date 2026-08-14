package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
)

func requestedOrderingFilterFixture() (
	expressions.Quantifier,
	*expressions.LogicalFilterExpression,
	*expressions.Reference,
) {
	q := requestedOrderingQuantifier("T", "filter_input")
	filter := mustRequestedOrderingConstruct(expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)}, q))
	return q, filter, expressions.InitialOf(filter)
}

func TestPushRequestedOrderingThroughFilter_PropagatesConstraint(t *testing.T) {
	t.Parallel()

	scanQ, filter, filterRef := requestedOrderingFilterFixture()
	id := requestedOrderingField(scanQ, "ID")

	cm := NewConstraintMap()
	ordering := properties.NewRequestedOrdering(
		[]properties.RequestedOrderingPart{
			{Value: id, SortOrder: properties.RequestedSortOrderAscending},
		},
		properties.DistinctnessNotDistinct, false,
	)
	Set(cm, filterRef, RequestedOrderingConstraintKey, []*properties.RequestedOrdering{ordering})

	rule := NewPushRequestedOrderingThroughFilterRule()
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), filter)
	if len(bindings) != 1 {
		t.Fatalf("matcher should match LogicalFilterExpression, got %d bindings", len(bindings))
	}

	call := &ImplementationRuleCall{
		Bindings:       bindings[0],
		Reference:      filterRef,
		Constraints:    cm,
		constraintOnly: true,
	}
	mustRunRequestedOrderingRule(t, rule, call)

	innerRef := filter.GetInner().GetRangesOver()
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
	assertRequestedOrderingField(t, parts[0].Value, id)
}

func TestPushRequestedOrderingThroughFilter_NoConstraintDoesNotPush(t *testing.T) {
	t.Parallel()

	_, filter, filterRef := requestedOrderingFilterFixture()

	cm := NewConstraintMap()

	rule := NewPushRequestedOrderingThroughFilterRule()
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), filter)
	call := &ImplementationRuleCall{
		Bindings:       bindings[0],
		Reference:      filterRef,
		Constraints:    cm,
		constraintOnly: true,
	}
	mustRunRequestedOrderingRule(t, rule, call)

	innerRef := filter.GetInner().GetRangesOver()
	_, ok := Get(cm, innerRef, RequestedOrderingConstraintKey)
	if ok {
		t.Fatal("constraint should not be pushed when parent has no constraint")
	}
}

func TestPushRequestedOrderingThroughFilter_NotConstraintOnlyDoesNotPush(t *testing.T) {
	t.Parallel()

	scanQ, filter, filterRef := requestedOrderingFilterFixture()
	id := requestedOrderingField(scanQ, "ID")

	cm := NewConstraintMap()
	ordering := properties.NewRequestedOrdering(
		[]properties.RequestedOrderingPart{
			{Value: id, SortOrder: properties.RequestedSortOrderAscending},
		},
		properties.DistinctnessNotDistinct, false,
	)
	Set(cm, filterRef, RequestedOrderingConstraintKey, []*properties.RequestedOrdering{ordering})

	rule := NewPushRequestedOrderingThroughFilterRule()
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), filter)
	call := &ImplementationRuleCall{
		Bindings:       bindings[0],
		Reference:      filterRef,
		Constraints:    cm,
		constraintOnly: false,
	}
	mustRunRequestedOrderingRule(t, rule, call)

	innerRef := filter.GetInner().GetRangesOver()
	_, ok := Get(cm, innerRef, RequestedOrderingConstraintKey)
	if ok {
		t.Fatal("constraint should not be pushed during implementation pass")
	}
}

func TestPushRequestedOrderingThroughFilter_NoYield(t *testing.T) {
	t.Parallel()

	scanQ, filter, filterRef := requestedOrderingFilterFixture()
	id := requestedOrderingField(scanQ, "ID")

	cm := NewConstraintMap()
	ordering := properties.NewRequestedOrdering(
		[]properties.RequestedOrderingPart{
			{Value: id, SortOrder: properties.RequestedSortOrderAscending},
		},
		properties.DistinctnessNotDistinct, false,
	)
	Set(cm, filterRef, RequestedOrderingConstraintKey, []*properties.RequestedOrdering{ordering})

	rule := NewPushRequestedOrderingThroughFilterRule()
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), filter)
	call := &ImplementationRuleCall{
		Bindings:       bindings[0],
		Reference:      filterRef,
		Constraints:    cm,
		constraintOnly: true,
	}
	mustRunRequestedOrderingRule(t, rule, call)

	if len(call.yielded) != 0 {
		t.Fatalf("constraint-push rule should not yield expressions, but yielded %d", len(call.yielded))
	}
}
