package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestPushRequestedOrderingThroughSort_PushesConstraint(t *testing.T) {
	t.Parallel()

	// Sort(col1 ASC) → Scan
	// The Sort rule creates the initial ordering constraint.
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: &values.FieldValue{Field: "col1", Typ: values.UnknownType}, Reverse: false},
		},
		scanQ,
	)
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
	rule.OnMatch(call)

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
	fv, ok := parts[0].Value.(*values.FieldValue)
	if !ok || fv.Field != "col1" {
		t.Fatalf("expected ordering on col1, got %v", parts[0].Value)
	}
	if parts[0].SortOrder != RequestedSortOrderAscending {
		t.Fatal("expected ASC sort order")
	}
}

func TestPushRequestedOrderingThroughSort_UnsortedDoesNotPush(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	sort := expressions.UnsortedLogicalSortExpression(scanQ)
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
	rule.OnMatch(call)

	innerRef := sort.GetInner().GetRangesOver()
	_, ok := Get(cm, innerRef, RequestedOrderingConstraintKey)
	if ok {
		t.Fatal("unsorted expression should not push any constraint")
	}
}

func TestPushRequestedOrderingThroughSort_NotConstraintOnlyDoesNotPush(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: &values.FieldValue{Field: "col1", Typ: values.UnknownType}, Reverse: false},
		},
		scanQ,
	)
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
	rule.OnMatch(call)

	innerRef := sort.GetInner().GetRangesOver()
	_, ok := Get(cm, innerRef, RequestedOrderingConstraintKey)
	if ok {
		t.Fatal("should not push during implementation pass")
	}
}

func TestPushRequestedOrderingThroughSort_DescKey(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: &values.FieldValue{Field: "a", Typ: values.UnknownType}, Reverse: true},
		},
		scanQ,
	)
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
	rule.OnMatch(call)

	innerRef := sort.GetInner().GetRangesOver()
	pushed, ok := Get(cm, innerRef, RequestedOrderingConstraintKey)
	if !ok {
		t.Fatal("constraint not pushed")
	}
	if pushed[0].GetParts()[0].SortOrder != RequestedSortOrderDescending {
		t.Fatal("expected DESC sort order")
	}
}

func TestPushRequestedOrderingThroughSort_MultipleSortKeys(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: &values.FieldValue{Field: "a", Typ: values.UnknownType}, Reverse: false},
			{Value: &values.FieldValue{Field: "b", Typ: values.UnknownType}, Reverse: true},
		},
		scanQ,
	)
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
	rule.OnMatch(call)

	innerRef := sort.GetInner().GetRangesOver()
	pushed, ok := Get(cm, innerRef, RequestedOrderingConstraintKey)
	if !ok {
		t.Fatal("constraint not pushed")
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

func TestPushRequestedOrderingThroughSort_NoYield(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: &values.FieldValue{Field: "a", Typ: values.UnknownType}, Reverse: false},
		},
		scanQ,
	)
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
	rule.OnMatch(call)

	if len(call.yielded) != 0 {
		t.Fatalf("constraint-push rule should not yield expressions, but yielded %d", len(call.yielded))
	}
}
