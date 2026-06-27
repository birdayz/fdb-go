package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestPushRequestedOrderingThroughUnion_PushesToAllBranches(t *testing.T) {
	t.Parallel()

	// Union(ScanA, ScanB)
	// Requested ordering: [col1 ASC]
	// Expected: ordering pushed to both branches.
	scanA := expressions.NewFullUnorderedScanExpression([]string{"A"}, values.UnknownType)
	scanAQ := expressions.ForEachQuantifier(expressions.InitialOf(scanA))
	scanB := expressions.NewFullUnorderedScanExpression([]string{"B"}, values.UnknownType)
	scanBQ := expressions.ForEachQuantifier(expressions.InitialOf(scanB))
	union := expressions.NewLogicalUnionExpression([]expressions.Quantifier{scanAQ, scanBQ})
	unionRef := expressions.InitialOf(union)

	cm := NewConstraintMap()
	reqOrd := NewRequestedOrdering([]RequestedOrderingPart{
		{Value: &values.FieldValue{Field: "col1", Typ: values.UnknownType}, SortOrder: RequestedSortOrderAscending},
	}, DistinctnessNotDistinct, false)
	Set(cm, unionRef, RequestedOrderingConstraintKey, []*RequestedOrdering{reqOrd})

	rule := NewPushRequestedOrderingThroughUnionRule()
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), union)
	if len(bindings) != 1 {
		t.Fatalf("matcher should match LogicalUnionExpression, got %d bindings", len(bindings))
	}

	call := &ImplementationRuleCall{
		Bindings:       bindings[0],
		Reference:      unionRef,
		Constraints:    cm,
		constraintOnly: true,
	}
	rule.OnMatch(call)

	children := union.GetQuantifiers()
	for i, child := range children {
		childRef := child.GetRangesOver()
		pushed, ok := Get(cm, childRef, RequestedOrderingConstraintKey)
		if !ok {
			t.Fatalf("constraint not pushed to child %d", i)
		}
		if len(pushed) != 1 {
			t.Fatalf("child %d: expected 1 pushed ordering, got %d", i, len(pushed))
		}
		parts := pushed[0].GetParts()
		if len(parts) != 1 {
			t.Fatalf("child %d: expected 1 ordering part, got %d", i, len(parts))
		}
		fv, ok := parts[0].Value.(*values.FieldValue)
		if !ok || fv.Field != "col1" {
			t.Fatalf("child %d: expected ordering on col1, got %v", i, parts[0].Value)
		}
		if parts[0].SortOrder != RequestedSortOrderAscending {
			t.Fatalf("child %d: expected ASC sort order", i)
		}
	}
}

func TestPushRequestedOrderingThroughUnion_FirstBranchExhaustive(t *testing.T) {
	t.Parallel()

	// Verify first branch gets exhaustive orderings, others get as-is.
	scanA := expressions.NewFullUnorderedScanExpression([]string{"A"}, values.UnknownType)
	scanAQ := expressions.ForEachQuantifier(expressions.InitialOf(scanA))
	scanB := expressions.NewFullUnorderedScanExpression([]string{"B"}, values.UnknownType)
	scanBQ := expressions.ForEachQuantifier(expressions.InitialOf(scanB))
	union := expressions.NewLogicalUnionExpression([]expressions.Quantifier{scanAQ, scanBQ})
	unionRef := expressions.InitialOf(union)

	cm := NewConstraintMap()
	reqOrd := NewRequestedOrdering([]RequestedOrderingPart{
		{Value: &values.FieldValue{Field: "a", Typ: values.UnknownType}, SortOrder: RequestedSortOrderAscending},
	}, DistinctnessNotDistinct, false) // not exhaustive
	Set(cm, unionRef, RequestedOrderingConstraintKey, []*RequestedOrdering{reqOrd})

	rule := NewPushRequestedOrderingThroughUnionRule()
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), union)
	call := &ImplementationRuleCall{
		Bindings:       bindings[0],
		Reference:      unionRef,
		Constraints:    cm,
		constraintOnly: true,
	}
	rule.OnMatch(call)

	children := union.GetQuantifiers()

	// First branch: exhaustive
	firstRef := children[0].GetRangesOver()
	firstPushed, ok := Get(cm, firstRef, RequestedOrderingConstraintKey)
	if !ok {
		t.Fatal("constraint not pushed to first branch")
	}
	if !firstPushed[0].IsExhaustive() {
		t.Fatal("first branch should receive exhaustive ordering")
	}

	// Second branch: original (not exhaustive)
	secondRef := children[1].GetRangesOver()
	secondPushed, ok := Get(cm, secondRef, RequestedOrderingConstraintKey)
	if !ok {
		t.Fatal("constraint not pushed to second branch")
	}
	if secondPushed[0].IsExhaustive() {
		t.Fatal("second branch should receive original (non-exhaustive) ordering")
	}
}

func TestPushRequestedOrderingThroughUnion_NoConstraintIsNoOp(t *testing.T) {
	t.Parallel()

	scanA := expressions.NewFullUnorderedScanExpression([]string{"A"}, values.UnknownType)
	scanAQ := expressions.ForEachQuantifier(expressions.InitialOf(scanA))
	scanB := expressions.NewFullUnorderedScanExpression([]string{"B"}, values.UnknownType)
	scanBQ := expressions.ForEachQuantifier(expressions.InitialOf(scanB))
	union := expressions.NewLogicalUnionExpression([]expressions.Quantifier{scanAQ, scanBQ})
	unionRef := expressions.InitialOf(union)

	cm := NewConstraintMap()
	// No ordering constraint set.

	rule := NewPushRequestedOrderingThroughUnionRule()
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), union)
	call := &ImplementationRuleCall{
		Bindings:       bindings[0],
		Reference:      unionRef,
		Constraints:    cm,
		constraintOnly: true,
	}
	rule.OnMatch(call)

	for i, child := range union.GetQuantifiers() {
		childRef := child.GetRangesOver()
		_, ok := Get(cm, childRef, RequestedOrderingConstraintKey)
		if ok {
			t.Fatalf("child %d: should not push when no ordering constraint exists", i)
		}
	}
}

func TestPushRequestedOrderingThroughUnion_NotConstraintOnlyIsNoOp(t *testing.T) {
	t.Parallel()

	scanA := expressions.NewFullUnorderedScanExpression([]string{"A"}, values.UnknownType)
	scanAQ := expressions.ForEachQuantifier(expressions.InitialOf(scanA))
	scanB := expressions.NewFullUnorderedScanExpression([]string{"B"}, values.UnknownType)
	scanBQ := expressions.ForEachQuantifier(expressions.InitialOf(scanB))
	union := expressions.NewLogicalUnionExpression([]expressions.Quantifier{scanAQ, scanBQ})
	unionRef := expressions.InitialOf(union)

	cm := NewConstraintMap()
	reqOrd := NewRequestedOrdering([]RequestedOrderingPart{
		{Value: &values.FieldValue{Field: "col1", Typ: values.UnknownType}, SortOrder: RequestedSortOrderAscending},
	}, DistinctnessNotDistinct, false)
	Set(cm, unionRef, RequestedOrderingConstraintKey, []*RequestedOrdering{reqOrd})

	rule := NewPushRequestedOrderingThroughUnionRule()
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), union)
	call := &ImplementationRuleCall{
		Bindings:       bindings[0],
		Reference:      unionRef,
		Constraints:    cm,
		constraintOnly: false,
	}
	rule.OnMatch(call)

	for i, child := range union.GetQuantifiers() {
		childRef := child.GetRangesOver()
		_, ok := Get(cm, childRef, RequestedOrderingConstraintKey)
		if ok {
			t.Fatalf("child %d: should not push during implementation pass", i)
		}
	}
}

func TestPushRequestedOrderingThroughUnion_ThreeBranches(t *testing.T) {
	t.Parallel()

	scanA := expressions.NewFullUnorderedScanExpression([]string{"A"}, values.UnknownType)
	scanAQ := expressions.ForEachQuantifier(expressions.InitialOf(scanA))
	scanB := expressions.NewFullUnorderedScanExpression([]string{"B"}, values.UnknownType)
	scanBQ := expressions.ForEachQuantifier(expressions.InitialOf(scanB))
	scanC := expressions.NewFullUnorderedScanExpression([]string{"C"}, values.UnknownType)
	scanCQ := expressions.ForEachQuantifier(expressions.InitialOf(scanC))
	union := expressions.NewLogicalUnionExpression([]expressions.Quantifier{scanAQ, scanBQ, scanCQ})
	unionRef := expressions.InitialOf(union)

	cm := NewConstraintMap()
	reqOrd := NewRequestedOrdering([]RequestedOrderingPart{
		{Value: &values.FieldValue{Field: "a", Typ: values.UnknownType}, SortOrder: RequestedSortOrderAscending},
		{Value: &values.FieldValue{Field: "b", Typ: values.UnknownType}, SortOrder: RequestedSortOrderDescending},
	}, DistinctnessNotDistinct, false)
	Set(cm, unionRef, RequestedOrderingConstraintKey, []*RequestedOrdering{reqOrd})

	rule := NewPushRequestedOrderingThroughUnionRule()
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), union)
	call := &ImplementationRuleCall{
		Bindings:       bindings[0],
		Reference:      unionRef,
		Constraints:    cm,
		constraintOnly: true,
	}
	rule.OnMatch(call)

	for i, child := range union.GetQuantifiers() {
		childRef := child.GetRangesOver()
		pushed, ok := Get(cm, childRef, RequestedOrderingConstraintKey)
		if !ok {
			t.Fatalf("constraint not pushed to child %d", i)
		}
		parts := pushed[0].GetParts()
		if len(parts) != 2 {
			t.Fatalf("child %d: expected 2 ordering parts, got %d", i, len(parts))
		}
	}
}

func TestPushRequestedOrderingThroughUnion_NoYield(t *testing.T) {
	t.Parallel()

	scanA := expressions.NewFullUnorderedScanExpression([]string{"A"}, values.UnknownType)
	scanAQ := expressions.ForEachQuantifier(expressions.InitialOf(scanA))
	scanB := expressions.NewFullUnorderedScanExpression([]string{"B"}, values.UnknownType)
	scanBQ := expressions.ForEachQuantifier(expressions.InitialOf(scanB))
	union := expressions.NewLogicalUnionExpression([]expressions.Quantifier{scanAQ, scanBQ})
	unionRef := expressions.InitialOf(union)

	cm := NewConstraintMap()
	reqOrd := NewRequestedOrdering([]RequestedOrderingPart{
		{Value: &values.FieldValue{Field: "col1", Typ: values.UnknownType}, SortOrder: RequestedSortOrderAscending},
	}, DistinctnessNotDistinct, false)
	Set(cm, unionRef, RequestedOrderingConstraintKey, []*RequestedOrdering{reqOrd})

	rule := NewPushRequestedOrderingThroughUnionRule()
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), union)
	call := &ImplementationRuleCall{
		Bindings:       bindings[0],
		Reference:      unionRef,
		Constraints:    cm,
		constraintOnly: true,
	}
	rule.OnMatch(call)

	if len(call.yielded) != 0 {
		t.Fatalf("constraint-push rule should not yield expressions, but yielded %d", len(call.yielded))
	}
}
