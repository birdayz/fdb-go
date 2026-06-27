package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestPushRequestedOrderingThroughGroupBy_AllKeysMatch(t *testing.T) {
	t.Parallel()

	// GroupBy(keys=[a, b], aggs=[SUM(v)])
	// Requested ordering: [a ASC, b DESC]
	// Expected: ordering [a ASC, b DESC] pushed (all keys consumed).
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	gb := expressions.NewGroupByExpression(
		[]values.Value{
			&values.FieldValue{Field: "a", Typ: values.UnknownType},
			&values.FieldValue{Field: "b", Typ: values.UnknownType},
		},
		[]expressions.AggregateSpec{
			{Function: expressions.AggSum, Operand: &values.FieldValue{Field: "v", Typ: values.UnknownType}},
		},
		scanQ,
	)
	gbRef := expressions.InitialOf(gb)

	cm := NewConstraintMap()
	reqOrd := NewRequestedOrdering([]RequestedOrderingPart{
		{Value: &values.FieldValue{Field: "a", Typ: values.UnknownType}, SortOrder: RequestedSortOrderAscending},
		{Value: &values.FieldValue{Field: "b", Typ: values.UnknownType}, SortOrder: RequestedSortOrderDescending},
	}, DistinctnessNotDistinct, false)
	Set(cm, gbRef, RequestedOrderingConstraintKey, []*RequestedOrdering{reqOrd})

	rule := NewPushRequestedOrderingThroughGroupByRule()
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), gb)
	if len(bindings) != 1 {
		t.Fatalf("matcher should match GroupByExpression, got %d bindings", len(bindings))
	}

	call := &ImplementationRuleCall{
		Bindings:       bindings[0],
		Reference:      gbRef,
		Constraints:    cm,
		constraintOnly: true,
	}
	rule.OnMatch(call)

	innerRef := gb.GetInner().GetRangesOver()
	pushed, ok := Get(cm, innerRef, RequestedOrderingConstraintKey)
	if !ok {
		t.Fatal("constraint not pushed to child Reference")
	}
	if len(pushed) != 1 {
		t.Fatalf("expected 1 pushed ordering, got %d", len(pushed))
	}
	parts := pushed[0].GetParts()
	if len(parts) != 2 {
		t.Fatalf("expected 2 ordering parts (all keys consumed), got %d", len(parts))
	}
	if fv := parts[0].Value.(*values.FieldValue); fv.Field != "a" || parts[0].SortOrder != RequestedSortOrderAscending {
		t.Fatalf("first part: want a ASC, got %s %v", fv.Field, parts[0].SortOrder)
	}
	if fv := parts[1].Value.(*values.FieldValue); fv.Field != "b" || parts[1].SortOrder != RequestedSortOrderDescending {
		t.Fatalf("second part: want b DESC, got %s %v", fv.Field, parts[1].SortOrder)
	}
}

func TestPushRequestedOrderingThroughGroupBy_PartialMatchAppendsRemaining(t *testing.T) {
	t.Parallel()

	// GroupBy(keys=[b, a, c])
	// Requested ordering: [a ASC]
	// Expected: [a ASC, b ANY, c ANY] — a matched, b and c appended.
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	gb := expressions.NewGroupByExpression(
		[]values.Value{
			&values.FieldValue{Field: "b", Typ: values.UnknownType},
			&values.FieldValue{Field: "a", Typ: values.UnknownType},
			&values.FieldValue{Field: "c", Typ: values.UnknownType},
		},
		[]expressions.AggregateSpec{
			{Function: expressions.AggCount, Operand: &values.FieldValue{Field: "id", Typ: values.UnknownType}},
		},
		scanQ,
	)
	gbRef := expressions.InitialOf(gb)

	cm := NewConstraintMap()
	reqOrd := NewRequestedOrdering([]RequestedOrderingPart{
		{Value: &values.FieldValue{Field: "a", Typ: values.UnknownType}, SortOrder: RequestedSortOrderAscending},
	}, DistinctnessNotDistinct, false)
	Set(cm, gbRef, RequestedOrderingConstraintKey, []*RequestedOrdering{reqOrd})

	rule := NewPushRequestedOrderingThroughGroupByRule()
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), gb)
	call := &ImplementationRuleCall{
		Bindings:       bindings[0],
		Reference:      gbRef,
		Constraints:    cm,
		constraintOnly: true,
	}
	rule.OnMatch(call)

	innerRef := gb.GetInner().GetRangesOver()
	pushed, ok := Get(cm, innerRef, RequestedOrderingConstraintKey)
	if !ok {
		t.Fatal("constraint not pushed to child Reference")
	}
	parts := pushed[0].GetParts()
	if len(parts) != 3 {
		t.Fatalf("expected 3 ordering parts (1 matched + 2 appended), got %d", len(parts))
	}
	// First: a ASC (from request)
	if fv := parts[0].Value.(*values.FieldValue); fv.Field != "a" || parts[0].SortOrder != RequestedSortOrderAscending {
		t.Fatalf("first part: want a ASC, got %s %v", fv.Field, parts[0].SortOrder)
	}
	// Second: b ANY (appended remaining)
	if fv := parts[1].Value.(*values.FieldValue); fv.Field != "b" || parts[1].SortOrder != RequestedSortOrderAny {
		t.Fatalf("second part: want b ANY, got %s %v", fv.Field, parts[1].SortOrder)
	}
	// Third: c ANY (appended remaining)
	if fv := parts[2].Value.(*values.FieldValue); fv.Field != "c" || parts[2].SortOrder != RequestedSortOrderAny {
		t.Fatalf("third part: want c ANY, got %s %v", fv.Field, parts[2].SortOrder)
	}
}

func TestPushRequestedOrderingThroughGroupBy_NonMatchingKeyDoesNotPush(t *testing.T) {
	t.Parallel()

	// GroupBy(keys=[a])
	// Requested ordering: [x ASC] — x is not a grouping key.
	// Expected: nothing pushed.
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	gb := expressions.NewGroupByExpression(
		[]values.Value{&values.FieldValue{Field: "a", Typ: values.UnknownType}},
		[]expressions.AggregateSpec{
			{Function: expressions.AggSum, Operand: &values.FieldValue{Field: "v", Typ: values.UnknownType}},
		},
		scanQ,
	)
	gbRef := expressions.InitialOf(gb)

	cm := NewConstraintMap()
	reqOrd := NewRequestedOrdering([]RequestedOrderingPart{
		{Value: &values.FieldValue{Field: "x", Typ: values.UnknownType}, SortOrder: RequestedSortOrderAscending},
	}, DistinctnessNotDistinct, false)
	Set(cm, gbRef, RequestedOrderingConstraintKey, []*RequestedOrdering{reqOrd})

	rule := NewPushRequestedOrderingThroughGroupByRule()
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), gb)
	call := &ImplementationRuleCall{
		Bindings:       bindings[0],
		Reference:      gbRef,
		Constraints:    cm,
		constraintOnly: true,
	}
	rule.OnMatch(call)

	innerRef := gb.GetInner().GetRangesOver()
	_, ok := Get(cm, innerRef, RequestedOrderingConstraintKey)
	if ok {
		t.Fatal("constraint should NOT be pushed when sort key doesn't match a grouping key")
	}
}

func TestPushRequestedOrderingThroughGroupBy_NoConstraintIsNoOp(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	gb := expressions.NewGroupByExpression(
		[]values.Value{&values.FieldValue{Field: "a", Typ: values.UnknownType}},
		[]expressions.AggregateSpec{
			{Function: expressions.AggCount, Operand: &values.FieldValue{Field: "id", Typ: values.UnknownType}},
		},
		scanQ,
	)
	gbRef := expressions.InitialOf(gb)

	cm := NewConstraintMap()
	// No ordering constraint set.

	rule := NewPushRequestedOrderingThroughGroupByRule()
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), gb)
	call := &ImplementationRuleCall{
		Bindings:       bindings[0],
		Reference:      gbRef,
		Constraints:    cm,
		constraintOnly: true,
	}
	rule.OnMatch(call)

	innerRef := gb.GetInner().GetRangesOver()
	_, ok := Get(cm, innerRef, RequestedOrderingConstraintKey)
	if ok {
		t.Fatal("should not push when no ordering constraint exists")
	}
}

func TestPushRequestedOrderingThroughGroupBy_NotConstraintOnlyIsNoOp(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	gb := expressions.NewGroupByExpression(
		[]values.Value{&values.FieldValue{Field: "a", Typ: values.UnknownType}},
		[]expressions.AggregateSpec{
			{Function: expressions.AggSum, Operand: &values.FieldValue{Field: "v", Typ: values.UnknownType}},
		},
		scanQ,
	)
	gbRef := expressions.InitialOf(gb)

	cm := NewConstraintMap()
	reqOrd := NewRequestedOrdering([]RequestedOrderingPart{
		{Value: &values.FieldValue{Field: "a", Typ: values.UnknownType}, SortOrder: RequestedSortOrderAscending},
	}, DistinctnessNotDistinct, false)
	Set(cm, gbRef, RequestedOrderingConstraintKey, []*RequestedOrdering{reqOrd})

	rule := NewPushRequestedOrderingThroughGroupByRule()
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), gb)
	call := &ImplementationRuleCall{
		Bindings:       bindings[0],
		Reference:      gbRef,
		Constraints:    cm,
		constraintOnly: false,
	}
	rule.OnMatch(call)

	innerRef := gb.GetInner().GetRangesOver()
	_, ok := Get(cm, innerRef, RequestedOrderingConstraintKey)
	if ok {
		t.Fatal("should not push during implementation pass (constraintOnly=false)")
	}
}

func TestPushRequestedOrderingThroughGroupBy_CaseInsensitiveMatch(t *testing.T) {
	t.Parallel()

	// GroupBy(keys=[col1]) with ordering [COL1 ASC] — case mismatch.
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	gb := expressions.NewGroupByExpression(
		[]values.Value{&values.FieldValue{Field: "col1", Typ: values.UnknownType}},
		[]expressions.AggregateSpec{
			{Function: expressions.AggSum, Operand: &values.FieldValue{Field: "col2", Typ: values.UnknownType}},
		},
		scanQ,
	)
	gbRef := expressions.InitialOf(gb)

	cm := NewConstraintMap()
	reqOrd := NewRequestedOrdering([]RequestedOrderingPart{
		{Value: &values.FieldValue{Field: "COL1", Typ: values.UnknownType}, SortOrder: RequestedSortOrderAscending},
	}, DistinctnessNotDistinct, false)
	Set(cm, gbRef, RequestedOrderingConstraintKey, []*RequestedOrdering{reqOrd})

	rule := NewPushRequestedOrderingThroughGroupByRule()
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), gb)
	call := &ImplementationRuleCall{
		Bindings:       bindings[0],
		Reference:      gbRef,
		Constraints:    cm,
		constraintOnly: true,
	}
	rule.OnMatch(call)

	innerRef := gb.GetInner().GetRangesOver()
	pushed, ok := Get(cm, innerRef, RequestedOrderingConstraintKey)
	if !ok {
		t.Fatal("constraint not pushed (case-insensitive match should work)")
	}
	if len(pushed) != 1 {
		t.Fatalf("expected 1 pushed ordering, got %d", len(pushed))
	}
}

func TestPushRequestedOrderingThroughGroupBy_EmptyGroupKeysPreserves(t *testing.T) {
	t.Parallel()

	// GroupBy(keys=[]) — scalar aggregation. Any ordering trivially satisfied.
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	gb := expressions.NewGroupByExpression(
		nil,
		[]expressions.AggregateSpec{
			{Function: expressions.AggCount, Operand: &values.FieldValue{Field: "id", Typ: values.UnknownType}},
		},
		scanQ,
	)
	gbRef := expressions.InitialOf(gb)

	cm := NewConstraintMap()
	reqOrd := NewRequestedOrdering([]RequestedOrderingPart{
		{Value: &values.FieldValue{Field: "x", Typ: values.UnknownType}, SortOrder: RequestedSortOrderAscending},
	}, DistinctnessNotDistinct, false)
	Set(cm, gbRef, RequestedOrderingConstraintKey, []*RequestedOrdering{reqOrd})

	rule := NewPushRequestedOrderingThroughGroupByRule()
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), gb)
	call := &ImplementationRuleCall{
		Bindings:       bindings[0],
		Reference:      gbRef,
		Constraints:    cm,
		constraintOnly: true,
	}
	rule.OnMatch(call)

	innerRef := gb.GetInner().GetRangesOver()
	pushed, ok := Get(cm, innerRef, RequestedOrderingConstraintKey)
	if !ok {
		t.Fatal("constraint should be pushed for scalar aggregation (preserve ordering)")
	}
	if !pushed[0].IsPreserve() {
		t.Fatal("scalar aggregation should push a preserve ordering")
	}
}

func TestPushRequestedOrderingThroughGroupBy_NoYield(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	gb := expressions.NewGroupByExpression(
		[]values.Value{&values.FieldValue{Field: "a", Typ: values.UnknownType}},
		[]expressions.AggregateSpec{
			{Function: expressions.AggSum, Operand: &values.FieldValue{Field: "v", Typ: values.UnknownType}},
		},
		scanQ,
	)
	gbRef := expressions.InitialOf(gb)

	cm := NewConstraintMap()
	reqOrd := NewRequestedOrdering([]RequestedOrderingPart{
		{Value: &values.FieldValue{Field: "a", Typ: values.UnknownType}, SortOrder: RequestedSortOrderAscending},
	}, DistinctnessNotDistinct, false)
	Set(cm, gbRef, RequestedOrderingConstraintKey, []*RequestedOrdering{reqOrd})

	rule := NewPushRequestedOrderingThroughGroupByRule()
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), gb)
	call := &ImplementationRuleCall{
		Bindings:       bindings[0],
		Reference:      gbRef,
		Constraints:    cm,
		constraintOnly: true,
	}
	rule.OnMatch(call)

	if len(call.yielded) != 0 {
		t.Fatalf("constraint-push rule should not yield expressions, but yielded %d", len(call.yielded))
	}
}
