package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
)

func TestPushRequestedOrderingThroughSort_PushesConstraint(t *testing.T) {
	t.Parallel()

	// Sort(col1 ASC) → Scan
	// The Sort rule creates the initial ordering constraint.
	scanQ := requestedOrderingQuantifier("T", "sort_input")
	col1 := requestedOrderingField(scanQ, "COL1")
	sort := mustRequestedOrderingConstruct(expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: col1, Reverse: false},
		},
		scanQ,
	))
	sortRef := expressions.InitialOf(sort)

	cm := NewConstraintMap()

	rule := NewPushRequestedOrderingThroughSortRule()
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), sort)
	if len(bindings) != 1 {
		t.Fatalf("matcher should match LogicalSortExpression, got %d bindings", len(bindings))
	}

	call := &ImplementationRuleCall{
		Bindings:       bindings[0],
		Reference:      sortRef,
		Constraints:    cm,
		constraintOnly: true,
	}
	mustRunRequestedOrderingRule(t, rule, call)

	// The constraint should be pushed to the inner (scan) Reference.
	innerRef := sort.GetInner().GetRangesOver()
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

func TestPushRequestedOrderingThroughSort_UnsortedDoesNotPush(t *testing.T) {
	t.Parallel()

	scanQ := requestedOrderingQuantifier("T", "sort_input")
	sort := mustRequestedOrderingConstruct(expressions.UnsortedLogicalSortExpression(scanQ))
	sortRef := expressions.InitialOf(sort)

	cm := NewConstraintMap()

	rule := NewPushRequestedOrderingThroughSortRule()
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), sort)
	call := &ImplementationRuleCall{
		Bindings:       bindings[0],
		Reference:      sortRef,
		Constraints:    cm,
		constraintOnly: true,
	}
	mustRunRequestedOrderingRule(t, rule, call)

	innerRef := sort.GetInner().GetRangesOver()
	_, ok := Get(cm, innerRef, RequestedOrderingConstraintKey)
	if ok {
		t.Fatal("unsorted expression should not push any constraint")
	}
}

func TestPushRequestedOrderingThroughSort_NotConstraintOnlyDoesNotPush(t *testing.T) {
	t.Parallel()

	scanQ := requestedOrderingQuantifier("T", "sort_input")
	col1 := requestedOrderingField(scanQ, "COL1")
	sort := mustRequestedOrderingConstruct(expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: col1, Reverse: false},
		},
		scanQ,
	))
	sortRef := expressions.InitialOf(sort)

	cm := NewConstraintMap()

	rule := NewPushRequestedOrderingThroughSortRule()
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), sort)
	call := &ImplementationRuleCall{
		Bindings:       bindings[0],
		Reference:      sortRef,
		Constraints:    cm,
		constraintOnly: false,
	}
	mustRunRequestedOrderingRule(t, rule, call)

	innerRef := sort.GetInner().GetRangesOver()
	_, ok := Get(cm, innerRef, RequestedOrderingConstraintKey)
	if ok {
		t.Fatal("should not push during implementation pass")
	}
}

func TestPushRequestedOrderingThroughSort_DescKey(t *testing.T) {
	t.Parallel()

	scanQ := requestedOrderingQuantifier("T", "sort_input")
	a := requestedOrderingField(scanQ, "A")
	sort := mustRequestedOrderingConstruct(expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: a, Reverse: true},
		},
		scanQ,
	))
	sortRef := expressions.InitialOf(sort)

	cm := NewConstraintMap()

	rule := NewPushRequestedOrderingThroughSortRule()
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), sort)
	call := &ImplementationRuleCall{
		Bindings:       bindings[0],
		Reference:      sortRef,
		Constraints:    cm,
		constraintOnly: true,
	}
	mustRunRequestedOrderingRule(t, rule, call)

	innerRef := sort.GetInner().GetRangesOver()
	pushed, ok := Get(cm, innerRef, RequestedOrderingConstraintKey)
	if !ok {
		t.Fatal("constraint not pushed")
	}
	if pushed[0].GetParts()[0].SortOrder != properties.RequestedSortOrderDescending {
		t.Fatal("expected DESC sort order")
	}
}

func TestPushRequestedOrderingThroughSort_MultipleSortKeys(t *testing.T) {
	t.Parallel()

	scanQ := requestedOrderingQuantifier("T", "sort_input")
	a := requestedOrderingField(scanQ, "A")
	b := requestedOrderingField(scanQ, "B")
	sort := mustRequestedOrderingConstruct(expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: a, Reverse: false},
			{Value: b, Reverse: true},
		},
		scanQ,
	))
	sortRef := expressions.InitialOf(sort)

	cm := NewConstraintMap()

	rule := NewPushRequestedOrderingThroughSortRule()
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), sort)
	call := &ImplementationRuleCall{
		Bindings:       bindings[0],
		Reference:      sortRef,
		Constraints:    cm,
		constraintOnly: true,
	}
	mustRunRequestedOrderingRule(t, rule, call)

	innerRef := sort.GetInner().GetRangesOver()
	pushed, ok := Get(cm, innerRef, RequestedOrderingConstraintKey)
	if !ok {
		t.Fatal("constraint not pushed")
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

func TestPushRequestedOrderingThroughSort_NoYield(t *testing.T) {
	t.Parallel()

	scanQ := requestedOrderingQuantifier("T", "sort_input")
	a := requestedOrderingField(scanQ, "A")
	sort := mustRequestedOrderingConstruct(expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: a, Reverse: false},
		},
		scanQ,
	))
	sortRef := expressions.InitialOf(sort)

	cm := NewConstraintMap()

	rule := NewPushRequestedOrderingThroughSortRule()
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), sort)
	call := &ImplementationRuleCall{
		Bindings:       bindings[0],
		Reference:      sortRef,
		Constraints:    cm,
		constraintOnly: true,
	}
	mustRunRequestedOrderingRule(t, rule, call)

	if len(call.yielded) != 0 {
		t.Fatalf("constraint-push rule should not yield expressions, but yielded %d", len(call.yielded))
	}
}
