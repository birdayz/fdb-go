package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

func TestPushRequestedOrderingThroughFilter_PropagatesConstraint(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	filter := expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{pT}, scanQ)
	filterRef := expressions.InitialOf(filter)

	cm := NewConstraintMap()
	ordering := NewRequestedOrdering(
		[]RequestedOrderingPart{
			{Value: &values.FieldValue{Field: "id", Typ: values.UnknownType}, SortOrder: RequestedSortOrderAscending},
		},
		DistinctnessNotDistinct, false,
	)
	Set(cm, filterRef, RequestedOrderingConstraintKey, []*RequestedOrdering{ordering})

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
	rule.OnMatch(call)

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
	fv, ok := parts[0].Value.(*values.FieldValue)
	if !ok || fv.Field != "id" {
		t.Fatalf("expected ordering on id, got %v", parts[0].Value)
	}
}

func TestPushRequestedOrderingThroughFilter_NoConstraintDoesNotPush(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	filter := expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{pT}, scanQ)
	filterRef := expressions.InitialOf(filter)

	cm := NewConstraintMap()

	rule := NewPushRequestedOrderingThroughFilterRule()
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), filter)
	call := &ImplementationRuleCall{
		Bindings:       bindings[0],
		Reference:      filterRef,
		Constraints:    cm,
		constraintOnly: true,
	}
	rule.OnMatch(call)

	innerRef := filter.GetInner().GetRangesOver()
	_, ok := Get(cm, innerRef, RequestedOrderingConstraintKey)
	if ok {
		t.Fatal("constraint should not be pushed when parent has no constraint")
	}
}

func TestPushRequestedOrderingThroughFilter_NotConstraintOnlyDoesNotPush(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	filter := expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{pT}, scanQ)
	filterRef := expressions.InitialOf(filter)

	cm := NewConstraintMap()
	ordering := NewRequestedOrdering(
		[]RequestedOrderingPart{
			{Value: &values.FieldValue{Field: "id", Typ: values.UnknownType}, SortOrder: RequestedSortOrderAscending},
		},
		DistinctnessNotDistinct, false,
	)
	Set(cm, filterRef, RequestedOrderingConstraintKey, []*RequestedOrdering{ordering})

	rule := NewPushRequestedOrderingThroughFilterRule()
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), filter)
	call := &ImplementationRuleCall{
		Bindings:       bindings[0],
		Reference:      filterRef,
		Constraints:    cm,
		constraintOnly: false,
	}
	rule.OnMatch(call)

	innerRef := filter.GetInner().GetRangesOver()
	_, ok := Get(cm, innerRef, RequestedOrderingConstraintKey)
	if ok {
		t.Fatal("constraint should not be pushed during implementation pass")
	}
}

func TestPushRequestedOrderingThroughFilter_NoYield(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	filter := expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{pT}, scanQ)
	filterRef := expressions.InitialOf(filter)

	cm := NewConstraintMap()
	ordering := NewRequestedOrdering(
		[]RequestedOrderingPart{
			{Value: &values.FieldValue{Field: "id", Typ: values.UnknownType}, SortOrder: RequestedSortOrderAscending},
		},
		DistinctnessNotDistinct, false,
	)
	Set(cm, filterRef, RequestedOrderingConstraintKey, []*RequestedOrdering{ordering})

	rule := NewPushRequestedOrderingThroughFilterRule()
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), filter)
	call := &ImplementationRuleCall{
		Bindings:       bindings[0],
		Reference:      filterRef,
		Constraints:    cm,
		constraintOnly: true,
	}
	rule.OnMatch(call)

	if len(call.yielded) != 0 {
		t.Fatalf("constraint-push rule should not yield expressions, but yielded %d", len(call.yielded))
	}
}
