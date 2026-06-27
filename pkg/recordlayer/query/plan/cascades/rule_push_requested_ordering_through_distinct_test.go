package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestPushRequestedOrderingThroughDistinct_PropagatesConstraint(t *testing.T) {
	t.Parallel()

	// Build: Distinct(Scan)
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	distinct := expressions.NewLogicalDistinctExpression(scanQ)
	distinctRef := expressions.InitialOf(distinct)

	// Set a requested ordering constraint on the Distinct's Reference
	// (as if pushed by a parent Sort rule).
	cm := NewConstraintMap()
	ordering := NewRequestedOrdering(
		[]RequestedOrderingPart{
			{Value: &values.FieldValue{Field: "col1", Typ: values.UnknownType}, SortOrder: RequestedSortOrderAscending},
		},
		DistinctnessNotDistinct, false,
	)
	Set(cm, distinctRef, RequestedOrderingConstraintKey, []*RequestedOrdering{ordering})

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
	rule.OnMatch(call)

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
	fv, ok := parts[0].Value.(*values.FieldValue)
	if !ok || fv.Field != "col1" {
		t.Fatalf("expected ordering on col1, got %v", parts[0].Value)
	}
	if parts[0].SortOrder != RequestedSortOrderAscending {
		t.Fatal("expected ASC sort order")
	}
}

func TestPushRequestedOrderingThroughDistinct_NoConstraintDoesNotPush(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	distinct := expressions.NewLogicalDistinctExpression(scanQ)
	distinctRef := expressions.InitialOf(distinct)

	cm := NewConstraintMap()

	rule := NewPushRequestedOrderingThroughDistinctRule()
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), distinct)
	call := &ImplementationRuleCall{
		Bindings:       bindings[0],
		Reference:      distinctRef,
		Constraints:    cm,
		constraintOnly: true,
	}
	rule.OnMatch(call)

	// No constraint set on parent → nothing pushed to child.
	innerRef := distinct.GetInner().GetRangesOver()
	_, ok := Get(cm, innerRef, RequestedOrderingConstraintKey)
	if ok {
		t.Fatal("constraint should not be pushed when parent has no constraint")
	}
}

func TestPushRequestedOrderingThroughDistinct_NotConstraintOnlyDoesNotPush(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	distinct := expressions.NewLogicalDistinctExpression(scanQ)
	distinctRef := expressions.InitialOf(distinct)

	cm := NewConstraintMap()
	ordering := NewRequestedOrdering(
		[]RequestedOrderingPart{
			{Value: &values.FieldValue{Field: "col1", Typ: values.UnknownType}, SortOrder: RequestedSortOrderAscending},
		},
		DistinctnessNotDistinct, false,
	)
	Set(cm, distinctRef, RequestedOrderingConstraintKey, []*RequestedOrdering{ordering})

	rule := NewPushRequestedOrderingThroughDistinctRule()
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), distinct)
	call := &ImplementationRuleCall{
		Bindings:       bindings[0],
		Reference:      distinctRef,
		Constraints:    cm,
		constraintOnly: false, // bottom-up implementation pass
	}
	rule.OnMatch(call)

	// constraintOnly=false → rule is a no-op, nothing pushed.
	innerRef := distinct.GetInner().GetRangesOver()
	_, ok := Get(cm, innerRef, RequestedOrderingConstraintKey)
	if ok {
		t.Fatal("constraint should not be pushed during implementation pass")
	}
}

func TestPushRequestedOrderingThroughDistinct_MultipleSortKeys(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	distinct := expressions.NewLogicalDistinctExpression(scanQ)
	distinctRef := expressions.InitialOf(distinct)

	cm := NewConstraintMap()
	ordering := NewRequestedOrdering(
		[]RequestedOrderingPart{
			{Value: &values.FieldValue{Field: "a", Typ: values.UnknownType}, SortOrder: RequestedSortOrderAscending},
			{Value: &values.FieldValue{Field: "b", Typ: values.UnknownType}, SortOrder: RequestedSortOrderDescending},
		},
		DistinctnessNotDistinct, false,
	)
	Set(cm, distinctRef, RequestedOrderingConstraintKey, []*RequestedOrdering{ordering})

	rule := NewPushRequestedOrderingThroughDistinctRule()
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), distinct)
	call := &ImplementationRuleCall{
		Bindings:       bindings[0],
		Reference:      distinctRef,
		Constraints:    cm,
		constraintOnly: true,
	}
	rule.OnMatch(call)

	innerRef := distinct.GetInner().GetRangesOver()
	pushed, ok := Get(cm, innerRef, RequestedOrderingConstraintKey)
	if !ok {
		t.Fatal("constraint not pushed to child Reference")
	}
	parts := pushed[0].GetParts()
	if len(parts) != 2 {
		t.Fatalf("expected 2 ordering parts, got %d", len(parts))
	}
	if fv := parts[0].Value.(*values.FieldValue); fv.Field != "a" || parts[0].SortOrder != RequestedSortOrderAscending {
		t.Fatalf("first part: want a ASC, got %s %v", fv.Field, parts[0].SortOrder)
	}
	if fv := parts[1].Value.(*values.FieldValue); fv.Field != "b" || parts[1].SortOrder != RequestedSortOrderDescending {
		t.Fatalf("second part: want b DESC, got %s %v", fv.Field, parts[1].SortOrder)
	}
}

func TestPushRequestedOrderingThroughDistinct_DescPreserved(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	distinct := expressions.NewLogicalDistinctExpression(scanQ)
	distinctRef := expressions.InitialOf(distinct)

	cm := NewConstraintMap()
	ordering := NewRequestedOrdering(
		[]RequestedOrderingPart{
			{Value: &values.FieldValue{Field: "a", Typ: values.UnknownType}, SortOrder: RequestedSortOrderDescending},
		},
		DistinctnessNotDistinct, false,
	)
	Set(cm, distinctRef, RequestedOrderingConstraintKey, []*RequestedOrdering{ordering})

	rule := NewPushRequestedOrderingThroughDistinctRule()
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), distinct)
	call := &ImplementationRuleCall{
		Bindings:       bindings[0],
		Reference:      distinctRef,
		Constraints:    cm,
		constraintOnly: true,
	}
	rule.OnMatch(call)

	innerRef := distinct.GetInner().GetRangesOver()
	pushed, ok := Get(cm, innerRef, RequestedOrderingConstraintKey)
	if !ok {
		t.Fatal("constraint not pushed to child Reference")
	}
	if pushed[0].GetParts()[0].SortOrder != RequestedSortOrderDescending {
		t.Fatal("DESC should be preserved through Distinct")
	}
}

func TestPushRequestedOrderingThroughDistinct_NoYield(t *testing.T) {
	t.Parallel()

	// The constraint-push rule should never yield expressions.
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	distinct := expressions.NewLogicalDistinctExpression(scanQ)
	distinctRef := expressions.InitialOf(distinct)

	cm := NewConstraintMap()
	ordering := NewRequestedOrdering(
		[]RequestedOrderingPart{
			{Value: &values.FieldValue{Field: "a", Typ: values.UnknownType}, SortOrder: RequestedSortOrderAscending},
		},
		DistinctnessNotDistinct, false,
	)
	Set(cm, distinctRef, RequestedOrderingConstraintKey, []*RequestedOrdering{ordering})

	rule := NewPushRequestedOrderingThroughDistinctRule()
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), distinct)
	call := &ImplementationRuleCall{
		Bindings:       bindings[0],
		Reference:      distinctRef,
		Constraints:    cm,
		constraintOnly: true,
	}
	rule.OnMatch(call)

	if len(call.yielded) != 0 {
		t.Fatalf("constraint-push rule should not yield expressions, but yielded %d", len(call.yielded))
	}
}
