package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func mustRequestedOrderingConstruct[T any](value T, err error) T {
	if err != nil {
		panic("construct requested-ordering fixture: " + err.Error())
	}
	return value
}

// requestedOrderingRowType is the shared exact input row for this propagation
// cluster. Every rule sees the same column layout; tests that cross a projection
// or aggregate boundary build a separate exact output row from the expression.
func requestedOrderingRowType() *values.RecordType {
	return values.NewRecordType("requested_ordering_row", false, []values.Field{
		{Name: "ID", FieldType: values.NullableLong},
		{Name: "COL1", FieldType: values.NullableLong},
		{Name: "A", FieldType: values.NullableLong},
		{Name: "B", FieldType: values.NullableLong},
		{Name: "C", FieldType: values.NullableLong},
		{Name: "V", FieldType: values.NullableLong},
		{Name: "X", FieldType: values.NullableLong},
		{Name: "Y", FieldType: values.NullableLong},
		{Name: "Z", FieldType: values.NullableLong},
		{Name: "REGION", FieldType: values.NullableLong},
		{Name: "AMOUNT", FieldType: values.NullableLong},
	})
}

func requestedOrderingScan(recordType string) *expressions.FullUnorderedScanExpression {
	return mustRequestedOrderingConstruct(expressions.NewFullUnorderedScanExpression(
		[]string{recordType}, requestedOrderingRowType()))
}

func requestedOrderingQuantifier(recordType, alias string) expressions.Quantifier {
	return expressions.NamedForEachQuantifier(
		values.NamedCorrelationIdentifier(alias),
		expressions.InitialOf(requestedOrderingScan(recordType)),
	)
}

func requestedOrderingFieldForType(
	alias values.CorrelationIdentifier,
	rowType values.Type,
	field string,
) values.Value {
	root := mustRequestedOrderingConstruct(values.NewQuantifiedObjectValue(alias, rowType))
	request := mustRequestedOrderingConstruct(values.FieldByName(field))
	return mustRequestedOrderingConstruct(values.ResolveFieldAccess(
		root, []values.FieldRequest{request}))
}

func requestedOrderingField(q expressions.Quantifier, field string) values.Value {
	return requestedOrderingFieldForType(
		q.GetAlias(),
		mustRequestedOrderingConstruct(q.GetFlowedObjectType()),
		field,
	)
}

func requestedOrderingOutputField(
	alias values.CorrelationIdentifier,
	resultValue values.Value,
	ordinal int,
) values.Value {
	root := mustRequestedOrderingConstruct(values.NewQuantifiedObjectValue(
		alias, resultValue.Type()))
	return mustRequestedOrderingConstruct(values.ResolveFieldOrdinals(
		root, []int{ordinal}))
}

func assertRequestedOrderingField(
	t testing.TB,
	got values.Value,
	want values.Value,
) {
	t.Helper()
	gotField, gotIsField := values.AsFieldValue(got)
	wantField, wantIsField := values.AsFieldValue(want)
	if !gotIsField || !wantIsField {
		t.Fatalf("ordering value = %T, want exact FieldValue %T", got, want)
	}
	gotIdentity, gotOK := values.OrderingIdentityOf(got)
	wantIdentity, wantOK := values.OrderingIdentityOf(want)
	if !gotOK || !wantOK || gotIdentity != wantIdentity {
		t.Fatalf("ordering field %q identity = %+v (ok=%v), want %q identity %+v (ok=%v)",
			gotField.DisplayName(), gotIdentity, gotOK,
			wantField.DisplayName(), wantIdentity, wantOK)
	}
}

// mustRunRequestedOrderingRule keeps the production stage/check/commit bridge
// visible in this self-contained cluster: constraint mutations are staged by
// OnMatch and become observable only after a successful rule call.
func mustRunRequestedOrderingRule(
	t testing.TB,
	rule ImplementationRule,
	call *ImplementationRuleCall,
) {
	t.Helper()
	rule.OnMatch(call)
	if err := call.Err(); err != nil {
		t.Fatalf("%T.OnMatch() unexpected error: %v", rule, err)
	}
	call.applyPendingConstraints()
}

func TestPushRequestedOrderingThroughDelete_PropagatesConstraint(t *testing.T) {
	t.Parallel()

	scanQ := requestedOrderingQuantifier("T", "delete_input")
	del := mustRequestedOrderingConstruct(expressions.NewDeleteExpression(scanQ, "MyRecord"))
	delRef := expressions.InitialOf(del)
	id := requestedOrderingField(scanQ, "ID")

	cm := NewConstraintMap()
	ordering := properties.NewRequestedOrdering(
		[]properties.RequestedOrderingPart{
			{Value: id, SortOrder: properties.RequestedSortOrderAscending},
		},
		properties.DistinctnessNotDistinct, false,
	)
	Set(cm, delRef, RequestedOrderingConstraintKey, []*properties.RequestedOrdering{ordering})

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
	mustRunRequestedOrderingRule(t, rule, call)

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
	assertRequestedOrderingField(t, parts[0].Value, id)
}

func TestPushRequestedOrderingThroughDelete_NoConstraintDoesNotPush(t *testing.T) {
	t.Parallel()

	scanQ := requestedOrderingQuantifier("T", "delete_input")
	del := mustRequestedOrderingConstruct(expressions.NewDeleteExpression(scanQ, "MyRecord"))
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
	mustRunRequestedOrderingRule(t, rule, call)

	innerRef := del.GetInner().GetRangesOver()
	_, ok := Get(cm, innerRef, RequestedOrderingConstraintKey)
	if ok {
		t.Fatal("constraint should not be pushed when parent has no constraint")
	}
}

func TestPushRequestedOrderingThroughDelete_NotConstraintOnlyDoesNotPush(t *testing.T) {
	t.Parallel()

	scanQ := requestedOrderingQuantifier("T", "delete_input")
	del := mustRequestedOrderingConstruct(expressions.NewDeleteExpression(scanQ, "MyRecord"))
	delRef := expressions.InitialOf(del)
	id := requestedOrderingField(scanQ, "ID")

	cm := NewConstraintMap()
	ordering := properties.NewRequestedOrdering(
		[]properties.RequestedOrderingPart{
			{Value: id, SortOrder: properties.RequestedSortOrderAscending},
		},
		properties.DistinctnessNotDistinct, false,
	)
	Set(cm, delRef, RequestedOrderingConstraintKey, []*properties.RequestedOrdering{ordering})

	rule := NewPushRequestedOrderingThroughDeleteRule()
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), del)
	call := &ImplementationRuleCall{
		Bindings:       bindings[0],
		Reference:      delRef,
		Constraints:    cm,
		constraintOnly: false,
	}
	mustRunRequestedOrderingRule(t, rule, call)

	innerRef := del.GetInner().GetRangesOver()
	_, ok := Get(cm, innerRef, RequestedOrderingConstraintKey)
	if ok {
		t.Fatal("constraint should not be pushed during implementation pass")
	}
}

func TestPushRequestedOrderingThroughDelete_NoYield(t *testing.T) {
	t.Parallel()

	scanQ := requestedOrderingQuantifier("T", "delete_input")
	del := mustRequestedOrderingConstruct(expressions.NewDeleteExpression(scanQ, "MyRecord"))
	delRef := expressions.InitialOf(del)
	id := requestedOrderingField(scanQ, "ID")

	cm := NewConstraintMap()
	ordering := properties.NewRequestedOrdering(
		[]properties.RequestedOrderingPart{
			{Value: id, SortOrder: properties.RequestedSortOrderAscending},
		},
		properties.DistinctnessNotDistinct, false,
	)
	Set(cm, delRef, RequestedOrderingConstraintKey, []*properties.RequestedOrdering{ordering})

	rule := NewPushRequestedOrderingThroughDeleteRule()
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), del)
	call := &ImplementationRuleCall{
		Bindings:       bindings[0],
		Reference:      delRef,
		Constraints:    cm,
		constraintOnly: true,
	}
	mustRunRequestedOrderingRule(t, rule, call)

	if len(call.yielded) != 0 {
		t.Fatalf("constraint-push rule should not yield expressions, but yielded %d", len(call.yielded))
	}
}
