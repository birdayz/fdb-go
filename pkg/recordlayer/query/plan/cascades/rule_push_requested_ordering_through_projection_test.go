package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
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
	reqOrd := properties.NewRequestedOrdering([]properties.RequestedOrderingPart{
		{
			// Output SLOT 0 of the projection. A resolved reference to a
			// projection output carries the ordinal; the name resolution that
			// turned `ORDER BY col1` into that slot happened upstream, at the one
			// place a name is legitimate (RFC-197).
			Value:     values.NewFieldValueWithResolvedOrdinal("COL1", 0, values.UnknownType),
			SortOrder: properties.RequestedSortOrderAscending,
		},
	}, properties.DistinctnessNotDistinct, false)
	Set(cm, projRef, RequestedOrderingConstraintKey, []*properties.RequestedOrdering{reqOrd})

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
	if parts[0].SortOrder != properties.RequestedSortOrderAscending {
		t.Fatal("expected ASC sort order")
	}
}

func TestPushRequestedOrderingThroughProjection_ComputedSlot(t *testing.T) {
	t.Parallel()

	// Projection: [A+B AS total, C AS c]
	// Requested ordering: [output slot 0 ASC]
	// Expected: ordering with the arithmetic expression (A+B) pushed.
	//
	// Was named for ALIAS RESOLUTION, which no longer happens here: the request
	// addresses the slot, and turning `ORDER BY total` into that slot is the
	// resolver's job upstream (RFC-197 item 3). What it pins now is that a
	// COMPUTED output slot pushes down to its whole expression, not just a bare
	// column read.
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
	reqOrd := properties.NewRequestedOrdering([]properties.RequestedOrderingPart{
		{
			Value:     values.NewFieldValueWithResolvedOrdinal("TOTAL", 0, values.UnknownType),
			SortOrder: properties.RequestedSortOrderAscending,
		},
	}, properties.DistinctnessNotDistinct, false)
	Set(cm, projRef, RequestedOrderingConstraintKey, []*properties.RequestedOrdering{reqOrd})

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
	// Requested ordering: [a LAZY "NONEXISTENT" carrier]
	// Rule should NOT push. Two independent reasons now, and both are load-bearing:
	// the projection has no such output, AND a lazy carrier has no ordinal to
	// select a slot with, so it declines even against a matching name. The second
	// case is covered on its own below.
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	proj := expressions.NewLogicalProjectionExpressionWithAliases(
		[]values.Value{&values.FieldValue{Field: "A", Typ: values.UnknownType}},
		[]string{"col1"},
		scanQ,
	)
	projRef := expressions.InitialOf(proj)

	cm := NewConstraintMap()
	reqOrd := properties.NewRequestedOrdering([]properties.RequestedOrderingPart{
		{
			Value:     &values.FieldValue{Field: "NONEXISTENT", Typ: values.UnknownType},
			SortOrder: properties.RequestedSortOrderAscending,
		},
	}, properties.DistinctnessNotDistinct, false)
	Set(cm, projRef, RequestedOrderingConstraintKey, []*properties.RequestedOrdering{reqOrd})

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
	reqOrd := properties.NewRequestedOrdering([]properties.RequestedOrderingPart{
		{
			Value:     &values.FieldValue{Field: "A", Typ: values.UnknownType},
			SortOrder: properties.RequestedSortOrderDescending,
		},
	}, properties.DistinctnessNotDistinct, false)
	Set(cm, projRef, RequestedOrderingConstraintKey, []*properties.RequestedOrdering{reqOrd})

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
	if pushed[0].GetParts()[0].SortOrder != properties.RequestedSortOrderDescending {
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
	reqOrd := properties.NewRequestedOrdering([]properties.RequestedOrderingPart{
		{
			Value:     values.NewFieldValueWithResolvedOrdinal("A", 0, values.UnknownType),
			SortOrder: properties.RequestedSortOrderAscending,
		},
		{
			Value:     values.NewFieldValueWithResolvedOrdinal("B", 1, values.UnknownType),
			SortOrder: properties.RequestedSortOrderDescending,
		},
	}, properties.DistinctnessNotDistinct, false)
	Set(cm, projRef, RequestedOrderingConstraintKey, []*properties.RequestedOrdering{reqOrd})

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
	if fv := parts[0].Value.(*values.FieldValue); fv.Field != "X" || parts[0].SortOrder != properties.RequestedSortOrderAscending {
		t.Fatalf("first part: want X ASC, got %s %v", fv.Field, parts[0].SortOrder)
	}
	if fv := parts[1].Value.(*values.FieldValue); fv.Field != "Y" || parts[1].SortOrder != properties.RequestedSortOrderDescending {
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
	reqOrd := properties.NewRequestedOrdering([]properties.RequestedOrderingPart{
		{
			Value:     &values.FieldValue{Field: "A", Typ: values.UnknownType},
			SortOrder: properties.RequestedSortOrderAscending,
		},
	}, properties.DistinctnessNotDistinct, false)
	Set(cm, projRef, RequestedOrderingConstraintKey, []*properties.RequestedOrdering{reqOrd})

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
	reqOrd := properties.NewRequestedOrdering([]properties.RequestedOrderingPart{
		{
			Value:     &values.FieldValue{Field: "A", Typ: values.UnknownType},
			SortOrder: properties.RequestedSortOrderAscending,
		},
	}, properties.DistinctnessNotDistinct, false)
	Set(cm, projRef, RequestedOrderingConstraintKey, []*properties.RequestedOrdering{reqOrd})

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

// TestPushRequestedOrderingThroughProjection_LazyRequestDoesNotPush is the
// dimension the conversion needed and nothing covered: a request that names the
// projection's output column CORRECTLY, but carries no ordinal, must still
// decline. Every other case here either matches by ordinal or misses by both, so
// none of them can tell an ordinal push-down from a name push-down.
//
// Declining is the fail-closed direction (a missed push, never a wrong slot), and
// it is what stops two same-named outputs of different projections from being one
// column. Resolving `ORDER BY col1` to a slot is the resolver's job, upstream and
// once (RFC-197).
func TestPushRequestedOrderingThroughProjection_LazyRequestDoesNotPush(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	proj := expressions.NewLogicalProjectionExpressionWithAliases(
		[]values.Value{&values.FieldValue{Field: "A", Typ: values.UnknownType}},
		[]string{"col1"},
		scanQ,
	)
	projRef := expressions.InitialOf(proj)

	cm := NewConstraintMap()
	reqOrd := properties.NewRequestedOrdering([]properties.RequestedOrderingPart{
		{
			// The RIGHT name, no ordinal.
			Value:     &values.FieldValue{Field: "COL1", Typ: values.UnknownType},
			SortOrder: properties.RequestedSortOrderAscending,
		},
	}, properties.DistinctnessNotDistinct, false)
	Set(cm, projRef, RequestedOrderingConstraintKey, []*properties.RequestedOrdering{reqOrd})

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
	if _, ok := Get(cm, innerRef, RequestedOrderingConstraintKey); ok {
		t.Fatal("a LAZY ordering request was pushed through the projection by matching its " +
			"display name against the output column list: the name resolution belongs " +
			"upstream, and a name-matched push conflates two same-named projection outputs")
	}
}
