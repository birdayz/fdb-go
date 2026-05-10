package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

func TestPushRequestedOrderingThroughDelete_PropagatesConstraint(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	del := expressions.NewDeleteExpression(scanQ, "MyRecord")
	delRef := expressions.InitialOf(del)

	cm := NewConstraintMap()
	ordering := NewRequestedOrdering(
		[]RequestedOrderingPart{
			{Value: &values.FieldValue{Field: "id", Typ: values.UnknownType}, SortOrder: RequestedSortOrderAscending},
		},
		DistinctnessNotDistinct, false,
	)
	Set(cm, delRef, RequestedOrderingConstraintKey, []*RequestedOrdering{ordering})

	rule := NewPushRequestedOrderingThroughDeleteRule()
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), del)
	if len(bindings) != 1 {
		t.Fatalf("matcher should match DeleteExpression, got %d bindings", len(bindings))
	}

	call := &ImplementationRuleCall{
		Bindings:       bindings[0],
		Reference:      delRef,
		Constraints:    cm,
		constraintOnly: true,
	}
	rule.OnMatch(call)

	innerRef := del.GetInner().GetRangesOver()
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

func TestPushRequestedOrderingThroughDelete_NoConstraintDoesNotPush(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	del := expressions.NewDeleteExpression(scanQ, "MyRecord")
	delRef := expressions.InitialOf(del)

	cm := NewConstraintMap()

	rule := NewPushRequestedOrderingThroughDeleteRule()
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), del)
	call := &ImplementationRuleCall{
		Bindings:       bindings[0],
		Reference:      delRef,
		Constraints:    cm,
		constraintOnly: true,
	}
	rule.OnMatch(call)

	innerRef := del.GetInner().GetRangesOver()
	_, ok := Get(cm, innerRef, RequestedOrderingConstraintKey)
	if ok {
		t.Fatal("constraint should not be pushed when parent has no constraint")
	}
}

func TestPushRequestedOrderingThroughDelete_NotConstraintOnlyDoesNotPush(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	del := expressions.NewDeleteExpression(scanQ, "MyRecord")
	delRef := expressions.InitialOf(del)

	cm := NewConstraintMap()
	ordering := NewRequestedOrdering(
		[]RequestedOrderingPart{
			{Value: &values.FieldValue{Field: "id", Typ: values.UnknownType}, SortOrder: RequestedSortOrderAscending},
		},
		DistinctnessNotDistinct, false,
	)
	Set(cm, delRef, RequestedOrderingConstraintKey, []*RequestedOrdering{ordering})

	rule := NewPushRequestedOrderingThroughDeleteRule()
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), del)
	call := &ImplementationRuleCall{
		Bindings:       bindings[0],
		Reference:      delRef,
		Constraints:    cm,
		constraintOnly: false,
	}
	rule.OnMatch(call)

	innerRef := del.GetInner().GetRangesOver()
	_, ok := Get(cm, innerRef, RequestedOrderingConstraintKey)
	if ok {
		t.Fatal("constraint should not be pushed during implementation pass")
	}
}

func TestPushRequestedOrderingThroughDelete_NoYield(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	del := expressions.NewDeleteExpression(scanQ, "MyRecord")
	delRef := expressions.InitialOf(del)

	cm := NewConstraintMap()
	ordering := NewRequestedOrdering(
		[]RequestedOrderingPart{
			{Value: &values.FieldValue{Field: "id", Typ: values.UnknownType}, SortOrder: RequestedSortOrderAscending},
		},
		DistinctnessNotDistinct, false,
	)
	Set(cm, delRef, RequestedOrderingConstraintKey, []*RequestedOrdering{ordering})

	rule := NewPushRequestedOrderingThroughDeleteRule()
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), del)
	call := &ImplementationRuleCall{
		Bindings:       bindings[0],
		Reference:      delRef,
		Constraints:    cm,
		constraintOnly: true,
	}
	rule.OnMatch(call)

	if len(call.yielded) != 0 {
		t.Fatalf("constraint-push rule should not yield expressions, but yielded %d", len(call.yielded))
	}
}
