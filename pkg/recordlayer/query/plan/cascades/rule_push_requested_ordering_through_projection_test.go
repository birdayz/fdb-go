package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestPushRequestedOrderingThroughProjection_PushesTranslatedOrdering(t *testing.T) {
	t.Parallel()

	// Projection: [A AS col1, B AS col2]
	// Requested ordering: [COL1 ASC]
	// Expected: ordering [A ASC] pushed to child Reference.
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	proj := expressions.NewLogicalProjectionExpressionWithAliases(
		[]values.Value{
			&values.FieldValue{Field: "A", Typ: values.UnknownType},
			&values.FieldValue{Field: "B", Typ: values.UnknownType},
		},
		[]string{"col1", "col2"},
		scanQ,
	)
	projRef := expressions.InitialOf(proj)

	// Set the ordering constraint on the projection's Reference.
	cm := NewConstraintMap()
	reqOrd := NewRequestedOrdering([]RequestedOrderingPart{
		{
			Value:     &values.FieldValue{Field: "COL1", Typ: values.UnknownType},
			SortOrder: RequestedSortOrderAscending,
		},
	}, DistinctnessNotDistinct, false)
	Set(cm, projRef, RequestedOrderingConstraintKey, []*RequestedOrdering{reqOrd})

	rule := NewPushRequestedOrderingThroughProjectionRule()
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), proj)
	if len(bindings) != 1 {
		t.Fatalf("matcher should match LogicalProjectionExpression, got %d bindings", len(bindings))
	}

	call := &ImplementationRuleCall{
		Bindings:       bindings[0],
		Reference:      projRef,
		Constraints:    cm,
		constraintOnly: true,
	}
	rule.OnMatch(call)

	// The constraint should be pushed to the inner (scan) Reference.
	innerRef := proj.GetInner().GetRangesOver()
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
	if !ok {
		t.Fatalf("expected FieldValue, got %T", parts[0].Value)
	}
	if fv.Field != "A" {
		t.Fatalf("expected translated field 'A', got %q", fv.Field)
	}
	if parts[0].SortOrder != RequestedSortOrderAscending {
		t.Fatal("expected ASC sort order")
	}
}

func TestPushRequestedOrderingThroughProjection_AliasResolution(t *testing.T) {
	t.Parallel()

	// Projection: [A+B AS total, C AS c]
	// Requested ordering: [TOTAL ASC]
	// Expected: ordering with arithmetic expression (A+B) pushed.
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	addExpr := &values.ArithmeticValue{
		Left:  &values.FieldValue{Field: "A", Typ: values.UnknownType},
		Right: &values.FieldValue{Field: "B", Typ: values.UnknownType},
		Op:    values.OpAdd,
	}
	proj := expressions.NewLogicalProjectionExpressionWithAliases(
		[]values.Value{addExpr, &values.FieldValue{Field: "C", Typ: values.UnknownType}},
		[]string{"total", "c"},
		scanQ,
	)
	projRef := expressions.InitialOf(proj)

	cm := NewConstraintMap()
	reqOrd := NewRequestedOrdering([]RequestedOrderingPart{
		{
			Value:     &values.FieldValue{Field: "TOTAL", Typ: values.UnknownType},
			SortOrder: RequestedSortOrderAscending,
		},
	}, DistinctnessNotDistinct, false)
	Set(cm, projRef, RequestedOrderingConstraintKey, []*RequestedOrdering{reqOrd})

	rule := NewPushRequestedOrderingThroughProjectionRule()
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), proj)
	call := &ImplementationRuleCall{
		Bindings:       bindings[0],
		Reference:      projRef,
		Constraints:    cm,
		constraintOnly: true,
	}
	rule.OnMatch(call)

	innerRef := proj.GetInner().GetRangesOver()
	pushed, ok := Get(cm, innerRef, RequestedOrderingConstraintKey)
	if !ok {
		t.Fatal("constraint not pushed")
	}
	parts := pushed[0].GetParts()
	if len(parts) != 1 {
		t.Fatalf("expected 1 ordering part, got %d", len(parts))
	}
	explain := values.ExplainValue(parts[0].Value)
	expectedExplain := values.ExplainValue(addExpr)
	if explain != expectedExplain {
		t.Fatalf("expected translated sort key %q, got %q", expectedExplain, explain)
	}
}

func TestPushRequestedOrderingThroughProjection_NoMatchDoesNotPush(t *testing.T) {
	t.Parallel()

	// Projection: [A AS col1]
	// Requested ordering: [NONEXISTENT ASC]
	// Rule should NOT push — key doesn't translate.
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	proj := expressions.NewLogicalProjectionExpressionWithAliases(
		[]values.Value{&values.FieldValue{Field: "A", Typ: values.UnknownType}},
		[]string{"col1"},
		scanQ,
	)
	projRef := expressions.InitialOf(proj)

	cm := NewConstraintMap()
	reqOrd := NewRequestedOrdering([]RequestedOrderingPart{
		{
			Value:     &values.FieldValue{Field: "NONEXISTENT", Typ: values.UnknownType},
			SortOrder: RequestedSortOrderAscending,
		},
	}, DistinctnessNotDistinct, false)
	Set(cm, projRef, RequestedOrderingConstraintKey, []*RequestedOrdering{reqOrd})

	rule := NewPushRequestedOrderingThroughProjectionRule()
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), proj)
	call := &ImplementationRuleCall{
		Bindings:       bindings[0],
		Reference:      projRef,
		Constraints:    cm,
		constraintOnly: true,
	}
	rule.OnMatch(call)

	innerRef := proj.GetInner().GetRangesOver()
	_, ok := Get(cm, innerRef, RequestedOrderingConstraintKey)
	if ok {
		t.Fatal("constraint should NOT be pushed when sort key doesn't translate")
	}
}

func TestPushRequestedOrderingThroughProjection_DescPreserved(t *testing.T) {
	t.Parallel()

	// Projection: [A AS a]
	// Requested ordering: [A DESC]
	// Expected: DESC preserved in pushed ordering.
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	proj := expressions.NewLogicalProjectionExpressionWithAliases(
		[]values.Value{&values.FieldValue{Field: "A", Typ: values.UnknownType}},
		[]string{"a"},
		scanQ,
	)
	projRef := expressions.InitialOf(proj)

	cm := NewConstraintMap()
	reqOrd := NewRequestedOrdering([]RequestedOrderingPart{
		{
			Value:     &values.FieldValue{Field: "A", Typ: values.UnknownType},
			SortOrder: RequestedSortOrderDescending,
		},
	}, DistinctnessNotDistinct, false)
	Set(cm, projRef, RequestedOrderingConstraintKey, []*RequestedOrdering{reqOrd})

	rule := NewPushRequestedOrderingThroughProjectionRule()
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), proj)
	call := &ImplementationRuleCall{
		Bindings:       bindings[0],
		Reference:      projRef,
		Constraints:    cm,
		constraintOnly: true,
	}
	rule.OnMatch(call)

	innerRef := proj.GetInner().GetRangesOver()
	pushed, ok := Get(cm, innerRef, RequestedOrderingConstraintKey)
	if !ok {
		t.Fatal("constraint not pushed")
	}
	if pushed[0].GetParts()[0].SortOrder != RequestedSortOrderDescending {
		t.Fatal("expected DESC sort order preserved")
	}
}

func TestPushRequestedOrderingThroughProjection_MultipleSortKeys(t *testing.T) {
	t.Parallel()

	// Projection: [X AS a, Y AS b, Z AS c]
	// Requested ordering: [A ASC, B DESC]
	// Expected: [X ASC, Y DESC] pushed to child.
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	proj := expressions.NewLogicalProjectionExpressionWithAliases(
		[]values.Value{
			&values.FieldValue{Field: "X", Typ: values.UnknownType},
			&values.FieldValue{Field: "Y", Typ: values.UnknownType},
			&values.FieldValue{Field: "Z", Typ: values.UnknownType},
		},
		[]string{"a", "b", "c"},
		scanQ,
	)
	projRef := expressions.InitialOf(proj)

	cm := NewConstraintMap()
	reqOrd := NewRequestedOrdering([]RequestedOrderingPart{
		{
			Value:     &values.FieldValue{Field: "A", Typ: values.UnknownType},
			SortOrder: RequestedSortOrderAscending,
		},
		{
			Value:     &values.FieldValue{Field: "B", Typ: values.UnknownType},
			SortOrder: RequestedSortOrderDescending,
		},
	}, DistinctnessNotDistinct, false)
	Set(cm, projRef, RequestedOrderingConstraintKey, []*RequestedOrdering{reqOrd})

	rule := NewPushRequestedOrderingThroughProjectionRule()
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), proj)
	call := &ImplementationRuleCall{
		Bindings:       bindings[0],
		Reference:      projRef,
		Constraints:    cm,
		constraintOnly: true,
	}
	rule.OnMatch(call)

	innerRef := proj.GetInner().GetRangesOver()
	pushed, ok := Get(cm, innerRef, RequestedOrderingConstraintKey)
	if !ok {
		t.Fatal("constraint not pushed")
	}
	parts := pushed[0].GetParts()
	if len(parts) != 2 {
		t.Fatalf("expected 2 ordering parts, got %d", len(parts))
	}
	if fv := parts[0].Value.(*values.FieldValue); fv.Field != "X" || parts[0].SortOrder != RequestedSortOrderAscending {
		t.Fatalf("first part: want X ASC, got %s %v", fv.Field, parts[0].SortOrder)
	}
	if fv := parts[1].Value.(*values.FieldValue); fv.Field != "Y" || parts[1].SortOrder != RequestedSortOrderDescending {
		t.Fatalf("second part: want Y DESC, got %s %v", fv.Field, parts[1].SortOrder)
	}
}

func TestPushRequestedOrderingThroughProjection_NotConstraintOnlyDoesNotPush(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	proj := expressions.NewLogicalProjectionExpressionWithAliases(
		[]values.Value{&values.FieldValue{Field: "A", Typ: values.UnknownType}},
		[]string{"a"},
		scanQ,
	)
	projRef := expressions.InitialOf(proj)

	cm := NewConstraintMap()
	reqOrd := NewRequestedOrdering([]RequestedOrderingPart{
		{
			Value:     &values.FieldValue{Field: "A", Typ: values.UnknownType},
			SortOrder: RequestedSortOrderAscending,
		},
	}, DistinctnessNotDistinct, false)
	Set(cm, projRef, RequestedOrderingConstraintKey, []*RequestedOrdering{reqOrd})

	rule := NewPushRequestedOrderingThroughProjectionRule()
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), proj)
	call := &ImplementationRuleCall{
		Bindings:       bindings[0],
		Reference:      projRef,
		Constraints:    cm,
		constraintOnly: false,
	}
	rule.OnMatch(call)

	innerRef := proj.GetInner().GetRangesOver()
	_, ok := Get(cm, innerRef, RequestedOrderingConstraintKey)
	if ok {
		t.Fatal("should not push during implementation pass")
	}
}

func TestPushRequestedOrderingThroughProjection_NoOrderingConstraint(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	proj := expressions.NewLogicalProjectionExpressionWithAliases(
		[]values.Value{&values.FieldValue{Field: "A", Typ: values.UnknownType}},
		[]string{"a"},
		scanQ,
	)
	projRef := expressions.InitialOf(proj)

	cm := NewConstraintMap()
	// No ordering constraint set.

	rule := NewPushRequestedOrderingThroughProjectionRule()
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), proj)
	call := &ImplementationRuleCall{
		Bindings:       bindings[0],
		Reference:      projRef,
		Constraints:    cm,
		constraintOnly: true,
	}
	rule.OnMatch(call)

	innerRef := proj.GetInner().GetRangesOver()
	_, ok := Get(cm, innerRef, RequestedOrderingConstraintKey)
	if ok {
		t.Fatal("should not push when no ordering constraint exists")
	}
}

func TestPushRequestedOrderingThroughProjection_NoYield(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	proj := expressions.NewLogicalProjectionExpressionWithAliases(
		[]values.Value{&values.FieldValue{Field: "A", Typ: values.UnknownType}},
		[]string{"a"},
		scanQ,
	)
	projRef := expressions.InitialOf(proj)

	cm := NewConstraintMap()
	reqOrd := NewRequestedOrdering([]RequestedOrderingPart{
		{
			Value:     &values.FieldValue{Field: "A", Typ: values.UnknownType},
			SortOrder: RequestedSortOrderAscending,
		},
	}, DistinctnessNotDistinct, false)
	Set(cm, projRef, RequestedOrderingConstraintKey, []*RequestedOrdering{reqOrd})

	rule := NewPushRequestedOrderingThroughProjectionRule()
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), proj)
	call := &ImplementationRuleCall{
		Bindings:       bindings[0],
		Reference:      projRef,
		Constraints:    cm,
		constraintOnly: true,
	}
	rule.OnMatch(call)

	if len(call.yielded) != 0 {
		t.Fatalf("constraint-push rule should not yield expressions, but yielded %d", len(call.yielded))
	}
}
