package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func requestedOrderingGroupByFixture(
	keys []string,
	function expressions.AggregateFunction,
	operand string,
) (*expressions.GroupByExpression, expressions.Quantifier, []values.Value) {
	input := requestedOrderingQuantifier("T", "groupby_input")
	groupingKeys := make([]values.Value, len(keys))
	for i, key := range keys {
		groupingKeys[i] = requestedOrderingField(input, key)
	}
	aggregates := []expressions.AggregateSpec{{
		Function: function,
		Operand:  requestedOrderingField(input, operand),
	}}
	groupBy := mustRequestedOrderingConstruct(expressions.NewGroupByExpression(
		groupingKeys, aggregates, input))
	return groupBy, input, groupingKeys
}

func requestedOrderingGroupByOutput(groupBy *expressions.GroupByExpression, ordinal int) values.Value {
	return requestedOrderingOutputField(
		values.NamedCorrelationIdentifier("groupby_output"),
		groupBy.GetResultValue(),
		ordinal,
	)
}

func runRequestedOrderingGroupBy(
	t *testing.T,
	groupBy *expressions.GroupByExpression,
	groupByRef *expressions.Reference,
	constraints *ConstraintMap,
	constraintOnly bool,
) *ImplementationRuleCall {
	t.Helper()
	rule := NewPushRequestedOrderingThroughGroupByRule()
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), groupBy)
	if len(bindings) != 1 {
		t.Fatalf("matcher bindings = %d, want one", len(bindings))
	}
	call := &ImplementationRuleCall{
		Bindings:       bindings[0],
		Reference:      groupByRef,
		Constraints:    constraints,
		constraintOnly: constraintOnly,
	}
	mustRunRequestedOrderingRule(t, rule, call)
	return call
}

func setRequestedOrdering(
	constraints *ConstraintMap,
	ref *expressions.Reference,
	parts []properties.RequestedOrderingPart,
) {
	Set(constraints, ref, RequestedOrderingConstraintKey, []*properties.RequestedOrdering{
		properties.NewRequestedOrdering(parts, properties.DistinctnessNotDistinct, false),
	})
}

func TestPushRequestedOrderingThroughGroupBy_AllKeysMatch(t *testing.T) {
	t.Parallel()
	_ = NewPushRequestedOrderingThroughGroupByRule() // direct behavioral-census anchor

	groupBy, _, groupingKeys := requestedOrderingGroupByFixture(
		[]string{"A", "B"}, expressions.AggSum, "V")
	groupByRef := expressions.InitialOf(groupBy)
	constraints := NewConstraintMap()
	setRequestedOrdering(constraints, groupByRef, []properties.RequestedOrderingPart{
		{Value: requestedOrderingGroupByOutput(groupBy, 0), SortOrder: properties.RequestedSortOrderAscending},
		{Value: requestedOrderingGroupByOutput(groupBy, 1), SortOrder: properties.RequestedSortOrderDescending},
	})
	runRequestedOrderingGroupBy(t, groupBy, groupByRef, constraints, true)

	pushed, ok := Get(constraints, groupBy.GetInner().GetRangesOver(), RequestedOrderingConstraintKey)
	if !ok || len(pushed) != 1 || len(pushed[0].GetParts()) != 2 {
		t.Fatalf("pushed orderings = %v ok=%v, want one two-part ordering", pushed, ok)
	}
	parts := pushed[0].GetParts()
	assertRequestedOrderingField(t, parts[0].Value, atQuantifierCurrent(t, groupBy.GetInner(), groupingKeys[0]))
	assertRequestedOrderingField(t, parts[1].Value, atQuantifierCurrent(t, groupBy.GetInner(), groupingKeys[1]))
	if parts[0].SortOrder != properties.RequestedSortOrderAscending ||
		parts[1].SortOrder != properties.RequestedSortOrderDescending {
		t.Fatalf("pushed directions = [%v, %v], want [ASC, DESC]",
			parts[0].SortOrder, parts[1].SortOrder)
	}
}

func TestPushRequestedOrderingThroughGroupBy_PartialMatchAppendsRemaining(t *testing.T) {
	t.Parallel()

	groupBy, _, groupingKeys := requestedOrderingGroupByFixture(
		[]string{"B", "A", "C"}, expressions.AggCount, "ID")
	groupByRef := expressions.InitialOf(groupBy)
	constraints := NewConstraintMap()
	setRequestedOrdering(constraints, groupByRef, []properties.RequestedOrderingPart{{
		Value:     requestedOrderingGroupByOutput(groupBy, 1),
		SortOrder: properties.RequestedSortOrderAscending,
	}})
	runRequestedOrderingGroupBy(t, groupBy, groupByRef, constraints, true)

	pushed, ok := Get(constraints, groupBy.GetInner().GetRangesOver(), RequestedOrderingConstraintKey)
	if !ok || len(pushed) != 1 || len(pushed[0].GetParts()) != 3 {
		t.Fatalf("pushed orderings = %v ok=%v, want one three-part ordering", pushed, ok)
	}
	parts := pushed[0].GetParts()
	assertRequestedOrderingField(t, parts[0].Value, atQuantifierCurrent(t, groupBy.GetInner(), groupingKeys[1]))
	assertRequestedOrderingField(t, parts[1].Value, atQuantifierCurrent(t, groupBy.GetInner(), groupingKeys[0]))
	assertRequestedOrderingField(t, parts[2].Value, atQuantifierCurrent(t, groupBy.GetInner(), groupingKeys[2]))
	if parts[0].SortOrder != properties.RequestedSortOrderAscending ||
		parts[1].SortOrder != properties.RequestedSortOrderAny ||
		parts[2].SortOrder != properties.RequestedSortOrderAny {
		t.Fatalf("pushed directions = [%v, %v, %v], want [ASC, ANY, ANY]",
			parts[0].SortOrder, parts[1].SortOrder, parts[2].SortOrder)
	}
}

func TestPushRequestedOrderingThroughGroupBy_NonMatchingKeyDoesNotPush(t *testing.T) {
	t.Parallel()

	groupBy, _, _ := requestedOrderingGroupByFixture([]string{"A"}, expressions.AggSum, "V")
	groupByRef := expressions.InitialOf(groupBy)
	foreign := requestedOrderingQuantifier("FOREIGN", "foreign_groupby_output")
	constraints := NewConstraintMap()
	setRequestedOrdering(constraints, groupByRef, []properties.RequestedOrderingPart{{
		Value:     requestedOrderingField(foreign, "X"),
		SortOrder: properties.RequestedSortOrderAscending,
	}})
	runRequestedOrderingGroupBy(t, groupBy, groupByRef, constraints, true)

	if _, ok := Get(constraints, groupBy.GetInner().GetRangesOver(), RequestedOrderingConstraintKey); ok {
		t.Fatal("constraint should not be pushed when the requested key is not a grouping key")
	}
}

func TestPushRequestedOrderingThroughGroupBy_NoConstraintIsNoOp(t *testing.T) {
	t.Parallel()

	groupBy, _, _ := requestedOrderingGroupByFixture([]string{"A"}, expressions.AggCount, "ID")
	groupByRef := expressions.InitialOf(groupBy)
	constraints := NewConstraintMap()
	runRequestedOrderingGroupBy(t, groupBy, groupByRef, constraints, true)
	if _, ok := Get(constraints, groupBy.GetInner().GetRangesOver(), RequestedOrderingConstraintKey); ok {
		t.Fatal("constraint should not be pushed when none exists above the group-by")
	}
}

func TestPushRequestedOrderingThroughGroupBy_NotConstraintOnlyIsNoOp(t *testing.T) {
	t.Parallel()

	groupBy, _, _ := requestedOrderingGroupByFixture([]string{"A"}, expressions.AggSum, "V")
	groupByRef := expressions.InitialOf(groupBy)
	constraints := NewConstraintMap()
	setRequestedOrdering(constraints, groupByRef, []properties.RequestedOrderingPart{{
		Value:     requestedOrderingGroupByOutput(groupBy, 0),
		SortOrder: properties.RequestedSortOrderAscending,
	}})
	runRequestedOrderingGroupBy(t, groupBy, groupByRef, constraints, false)
	if _, ok := Get(constraints, groupBy.GetInner().GetRangesOver(), RequestedOrderingConstraintKey); ok {
		t.Fatal("constraint should not be pushed during the implementation pass")
	}
}

func TestPushRequestedOrderingThroughGroupBy_CaseInsensitiveMatch(t *testing.T) {
	t.Parallel()

	lowerRow := values.NewRecordType("groupby_lower_row", false, []values.Field{
		{Name: "col1", FieldType: values.NullableLong},
		{Name: "col2", FieldType: values.NullableLong},
	})
	scan := mustRequestedOrderingConstruct(expressions.NewFullUnorderedScanExpression(
		[]string{"T"}, lowerRow))
	input := expressions.NamedForEachQuantifier(
		values.NamedCorrelationIdentifier("groupby_lower_input"), expressions.InitialOf(scan))
	groupingKey := requestedOrderingField(input, "col1")
	groupBy := mustRequestedOrderingConstruct(expressions.NewGroupByExpression(
		[]values.Value{groupingKey},
		[]expressions.AggregateSpec{{Function: expressions.AggSum, Operand: requestedOrderingField(input, "col2")}},
		input,
	))
	upperRow := values.NewRecordType("groupby_upper_output", false, []values.Field{
		{Name: "COL1", FieldType: values.NullableLong},
	})
	request := requestedOrderingFieldForType(
		values.NamedCorrelationIdentifier("groupby_output"), upperRow, "COL1")
	groupByRef := expressions.InitialOf(groupBy)
	constraints := NewConstraintMap()
	setRequestedOrdering(constraints, groupByRef, []properties.RequestedOrderingPart{{
		Value: request, SortOrder: properties.RequestedSortOrderAscending,
	}})
	runRequestedOrderingGroupBy(t, groupBy, groupByRef, constraints, true)

	pushed, ok := Get(constraints, groupBy.GetInner().GetRangesOver(), RequestedOrderingConstraintKey)
	if !ok || len(pushed) != 1 || len(pushed[0].GetParts()) != 1 {
		t.Fatalf("pushed orderings = %v ok=%v, want one case-insensitively matched ordering", pushed, ok)
	}
	assertRequestedOrderingField(t, pushed[0].GetParts()[0].Value, atQuantifierCurrent(t, groupBy.GetInner(), groupingKey))
}

func TestPushRequestedOrderingThroughGroupBy_EmptyGroupKeysPreserves(t *testing.T) {
	t.Parallel()

	groupBy, _, _ := requestedOrderingGroupByFixture(nil, expressions.AggCount, "ID")
	groupByRef := expressions.InitialOf(groupBy)
	foreign := requestedOrderingQuantifier("FOREIGN", "scalar_groupby_output")
	constraints := NewConstraintMap()
	setRequestedOrdering(constraints, groupByRef, []properties.RequestedOrderingPart{{
		Value: requestedOrderingField(foreign, "X"), SortOrder: properties.RequestedSortOrderAscending,
	}})
	runRequestedOrderingGroupBy(t, groupBy, groupByRef, constraints, true)

	pushed, ok := Get(constraints, groupBy.GetInner().GetRangesOver(), RequestedOrderingConstraintKey)
	if !ok || len(pushed) != 1 || !pushed[0].IsPreserve() {
		t.Fatalf("scalar group-by pushed orderings = %v ok=%v, want preserve", pushed, ok)
	}
}

func TestPushRequestedOrderingThroughGroupBy_NoYield(t *testing.T) {
	t.Parallel()

	groupBy, _, _ := requestedOrderingGroupByFixture([]string{"A"}, expressions.AggSum, "V")
	groupByRef := expressions.InitialOf(groupBy)
	constraints := NewConstraintMap()
	setRequestedOrdering(constraints, groupByRef, []properties.RequestedOrderingPart{{
		Value: requestedOrderingGroupByOutput(groupBy, 0), SortOrder: properties.RequestedSortOrderAscending,
	}})
	call := runRequestedOrderingGroupBy(t, groupBy, groupByRef, constraints, true)
	if len(call.yielded) != 0 {
		t.Fatalf("constraint-push rule yielded %d expressions, want none", len(call.yielded))
	}
}

// This pins the row-space translation: the request is rooted in the aggregate
// output, while the propagated key must be the exact input field used to group.
func TestPushRequestedOrderingThroughGroupBy_PushesTheGroupingKeyNotTheRequest(t *testing.T) {
	t.Parallel()

	inputRow := values.NewRecordType("groupby_identity_input", false, []values.Field{
		{Name: "K", FieldType: values.NullableLong},
		{Name: "V", FieldType: values.NullableLong},
	})
	scan := mustRequestedOrderingConstruct(expressions.NewFullUnorderedScanExpression(
		[]string{"T"}, inputRow))
	input := expressions.NamedForEachQuantifier(
		values.NamedCorrelationIdentifier("groupby_identity_input"), expressions.InitialOf(scan))
	groupingKey := requestedOrderingField(input, "K")
	groupBy := mustRequestedOrderingConstruct(expressions.NewGroupByExpression(
		[]values.Value{groupingKey},
		[]expressions.AggregateSpec{{Function: expressions.AggSum, Operand: requestedOrderingField(input, "V")}},
		input,
	))
	requestedValue := requestedOrderingGroupByOutput(groupBy, 0)
	requestedIdentity, requestOK := values.OrderingIdentityOf(requestedValue)
	groupingIdentity, groupingOK := values.OrderingIdentityOf(groupingKey)
	if !requestOK || !groupingOK || requestedIdentity == groupingIdentity {
		t.Fatalf("fixture identities request=%+v(ok=%v) grouping=%+v(ok=%v), want distinct exact rows",
			requestedIdentity, requestOK, groupingIdentity, groupingOK)
	}

	groupByRef := expressions.InitialOf(groupBy)
	constraints := NewConstraintMap()
	setRequestedOrdering(constraints, groupByRef, []properties.RequestedOrderingPart{{
		Value: requestedValue, SortOrder: properties.RequestedSortOrderDescending,
	}})
	runRequestedOrderingGroupBy(t, groupBy, groupByRef, constraints, true)
	pushed, ok := Get(constraints, groupBy.GetInner().GetRangesOver(), RequestedOrderingConstraintKey)
	if !ok || len(pushed) != 1 || len(pushed[0].GetParts()) != 1 {
		t.Fatalf("pushed orderings = %v ok=%v, want one part", pushed, ok)
	}
	part := pushed[0].GetParts()[0]
	assertRequestedOrderingField(t, part.Value, atQuantifierCurrent(t, groupBy.GetInner(), groupingKey))
	if part.SortOrder != properties.RequestedSortOrderDescending {
		t.Fatalf("pushed direction = %v, want DESC", part.SortOrder)
	}
}

// atQuantifierCurrent restates a value spelled over quantifier q in q's
// current-row space — the space a constraint on q's Reference is stored in,
// and the space the group-by rule rebases its synthesized ordering into.
func atQuantifierCurrent(t *testing.T, q expressions.Quantifier, v values.Value) values.Value {
	t.Helper()
	rebased, err := requestedOrderingAtInnerCurrent(properties.NewRequestedOrdering(
		[]properties.RequestedOrderingPart{{Value: v, SortOrder: properties.RequestedSortOrderAscending}},
		properties.DistinctnessPreserveDistinctness, false), q)
	if err != nil {
		t.Fatalf("rebase into the quantifier's current-row space: %v", err)
	}
	return rebased.GetParts()[0].Value
}
