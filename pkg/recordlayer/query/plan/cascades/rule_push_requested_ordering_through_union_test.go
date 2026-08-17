package cascades

import (
	"fmt"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func requestedOrderingUnionFixture(recordTypes ...string) (
	*expressions.LogicalUnionExpression,
	*expressions.Reference,
) {
	quantifiers := make([]expressions.Quantifier, len(recordTypes))
	for i, recordType := range recordTypes {
		quantifiers[i] = requestedOrderingQuantifier(
			recordType, fmt.Sprintf("union_input_%d", i))
	}
	union := mustRequestedOrderingConstruct(expressions.NewLogicalUnionExpression(quantifiers))
	return union, expressions.InitialOf(union)
}

func requestedOrderingUnionField(
	union *expressions.LogicalUnionExpression,
	field string,
) values.Value {
	return requestedOrderingFieldForType(
		values.NamedCorrelationIdentifier("union_output"),
		union.GetResultValue().Type(),
		field,
	)
}

func TestPushRequestedOrderingThroughUnion_PushesToAllBranches(t *testing.T) {
	t.Parallel()

	// Union(ScanA, ScanB)
	// Requested ordering: [col1 ASC]
	// Expected: ordering pushed to both branches.
	union, unionRef := requestedOrderingUnionFixture("A", "B")
	col1 := requestedOrderingUnionField(union, "COL1")

	cm := NewConstraintMap()
	reqOrd := properties.NewRequestedOrdering([]properties.RequestedOrderingPart{
		{Value: col1, SortOrder: properties.RequestedSortOrderAscending},
	}, properties.DistinctnessNotDistinct, false)
	Set(cm, unionRef, RequestedOrderingConstraintKey, []*properties.RequestedOrdering{reqOrd})

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
	mustRunRequestedOrderingRule(t, rule, call)

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
		assertRequestedOrderingField(t, parts[0].Value, col1)
		if parts[0].SortOrder != properties.RequestedSortOrderAscending {
			t.Fatalf("child %d: expected ASC sort order", i)
		}
	}
}

func TestPushRequestedOrderingThroughUnion_FirstBranchExhaustive(t *testing.T) {
	t.Parallel()

	// Verify first branch gets exhaustive orderings, others get as-is.
	union, unionRef := requestedOrderingUnionFixture("A", "B")
	a := requestedOrderingUnionField(union, "A")

	cm := NewConstraintMap()
	reqOrd := properties.NewRequestedOrdering([]properties.RequestedOrderingPart{
		{Value: a, SortOrder: properties.RequestedSortOrderAscending},
	}, properties.DistinctnessNotDistinct, false) // not exhaustive
	Set(cm, unionRef, RequestedOrderingConstraintKey, []*properties.RequestedOrdering{reqOrd})

	rule := NewPushRequestedOrderingThroughUnionRule()
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), union)
	call := &ImplementationRuleCall{
		Bindings:       bindings[0],
		Reference:      unionRef,
		Constraints:    cm,
		constraintOnly: true,
	}
	mustRunRequestedOrderingRule(t, rule, call)

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

	union, unionRef := requestedOrderingUnionFixture("A", "B")

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
	mustRunRequestedOrderingRule(t, rule, call)

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

	union, unionRef := requestedOrderingUnionFixture("A", "B")
	col1 := requestedOrderingUnionField(union, "COL1")

	cm := NewConstraintMap()
	reqOrd := properties.NewRequestedOrdering([]properties.RequestedOrderingPart{
		{Value: col1, SortOrder: properties.RequestedSortOrderAscending},
	}, properties.DistinctnessNotDistinct, false)
	Set(cm, unionRef, RequestedOrderingConstraintKey, []*properties.RequestedOrdering{reqOrd})

	rule := NewPushRequestedOrderingThroughUnionRule()
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), union)
	call := &ImplementationRuleCall{
		Bindings:       bindings[0],
		Reference:      unionRef,
		Constraints:    cm,
		constraintOnly: false,
	}
	mustRunRequestedOrderingRule(t, rule, call)

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

	union, unionRef := requestedOrderingUnionFixture("A", "B", "C")
	a := requestedOrderingUnionField(union, "A")
	b := requestedOrderingUnionField(union, "B")

	cm := NewConstraintMap()
	reqOrd := properties.NewRequestedOrdering([]properties.RequestedOrderingPart{
		{Value: a, SortOrder: properties.RequestedSortOrderAscending},
		{Value: b, SortOrder: properties.RequestedSortOrderDescending},
	}, properties.DistinctnessNotDistinct, false)
	Set(cm, unionRef, RequestedOrderingConstraintKey, []*properties.RequestedOrdering{reqOrd})

	rule := NewPushRequestedOrderingThroughUnionRule()
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), union)
	call := &ImplementationRuleCall{
		Bindings:       bindings[0],
		Reference:      unionRef,
		Constraints:    cm,
		constraintOnly: true,
	}
	mustRunRequestedOrderingRule(t, rule, call)

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

	union, unionRef := requestedOrderingUnionFixture("A", "B")
	col1 := requestedOrderingUnionField(union, "COL1")

	cm := NewConstraintMap()
	reqOrd := properties.NewRequestedOrdering([]properties.RequestedOrderingPart{
		{Value: col1, SortOrder: properties.RequestedSortOrderAscending},
	}, properties.DistinctnessNotDistinct, false)
	Set(cm, unionRef, RequestedOrderingConstraintKey, []*properties.RequestedOrdering{reqOrd})

	rule := NewPushRequestedOrderingThroughUnionRule()
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), union)
	call := &ImplementationRuleCall{
		Bindings:       bindings[0],
		Reference:      unionRef,
		Constraints:    cm,
		constraintOnly: true,
	}
	mustRunRequestedOrderingRule(t, rule, call)

	if len(call.yielded) != 0 {
		t.Fatalf("constraint-push rule should not yield expressions, but yielded %d", len(call.yielded))
	}
}
