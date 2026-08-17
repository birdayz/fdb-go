package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func mustDMLOrderingConstruct[T any](value T, err error) T {
	if err != nil {
		panic("construct DML-ordering fixture: " + err.Error())
	}
	return value
}

func dmlOrderingRowType() *values.RecordType {
	return values.NewRecordType("dml_ordering_row", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong},
		{Name: "NAME", FieldType: values.NullableString},
		{Name: "COL1", FieldType: values.NullableLong},
	})
}

func dmlOrderingQuantifier(alias string) expressions.Quantifier {
	scan := mustDMLOrderingConstruct(expressions.NewFullUnorderedScanExpression(
		[]string{"T"}, dmlOrderingRowType()))
	return expressions.NamedForEachQuantifier(
		values.NamedCorrelationIdentifier(alias), expressions.InitialOf(scan))
}

func dmlOrderingField(q expressions.Quantifier, ordinals ...int) values.Value {
	root := mustDMLOrderingConstruct(q.RequireFlowedObjectValue())
	return mustDMLOrderingConstruct(values.ResolveFieldOrdinals(root, ordinals))
}

func dmlOrderingOutputField(
	upperAlias values.CorrelationIdentifier,
	result values.Value,
	ordinals ...int,
) values.Value {
	root := mustDMLOrderingConstruct(values.NewQuantifiedObjectValue(
		upperAlias, result.Type()))
	return mustDMLOrderingConstruct(values.ResolveFieldOrdinals(root, ordinals))
}

func dmlRequestedOrdering(value values.Value, sortOrder properties.RequestedSortOrder) *properties.RequestedOrdering {
	return properties.NewRequestedOrdering(
		[]properties.RequestedOrderingPart{{Value: value, SortOrder: sortOrder}},
		properties.DistinctnessNotDistinct,
		false,
	)
}

func runDMLOrderingRule(
	t testing.TB,
	rule ImplementationRule,
	expr expressions.RelationalExpression,
	ref *expressions.Reference,
	constraints *ConstraintMap,
	constraintOnly bool,
) *ImplementationRuleCall {
	t.Helper()
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), expr)
	if len(bindings) != 1 {
		t.Fatalf("%T matcher produced %d bindings, want 1", rule, len(bindings))
	}
	call := &ImplementationRuleCall{
		Bindings:       bindings[0],
		Reference:      ref,
		Constraints:    constraints,
		constraintOnly: constraintOnly,
	}
	rule.OnMatch(call)
	if err := call.Err(); err != nil {
		t.Fatalf("%T.OnMatch: %v", rule, err)
	}
	call.applyPendingConstraints()
	return call
}

func assertDMLOrderingField(
	t testing.TB,
	got values.Value,
	want values.Value,
) {
	t.Helper()
	gotField, gotOK := values.AsFieldValue(got)
	wantField, wantOK := values.AsFieldValue(want)
	if !gotOK || !wantOK {
		t.Fatalf("ordering value = %T, want exact FieldValue %T", got, want)
	}
	gotIdentity, gotIdentityOK := values.OrderingIdentityOf(got)
	wantIdentity, wantIdentityOK := values.OrderingIdentityOf(want)
	if !gotIdentityOK || !wantIdentityOK || gotIdentity != wantIdentity {
		t.Fatalf("ordering field %q identity = %+v, want %q identity %+v",
			gotField.DisplayName(), gotIdentity,
			wantField.DisplayName(), wantIdentity)
	}
}

func pushedDMLOrdering(
	t testing.TB,
	constraints *ConstraintMap,
	innerRef *expressions.Reference,
) *properties.RequestedOrdering {
	t.Helper()
	pushed, ok := Get(constraints, innerRef, RequestedOrderingConstraintKey)
	if !ok {
		t.Fatal("constraint not pushed to child Reference")
	}
	if len(pushed) != 1 {
		t.Fatalf("pushed orderings = %d, want 1", len(pushed))
	}
	return pushed[0]
}

// --- Insert ---

func TestPushRequestedOrderingThroughInsert_PropagatesConstraint(t *testing.T) {
	t.Parallel()

	inputQ := dmlOrderingQuantifier("insert_input")
	insert := mustDMLOrderingConstruct(expressions.NewInsertExpression(
		inputQ, "MyRecord", dmlOrderingRowType()))
	insertRef := expressions.InitialOf(insert)
	id := dmlOrderingField(inputQ, 0)

	constraints := NewConstraintMap()
	Set(constraints, insertRef, RequestedOrderingConstraintKey,
		[]*properties.RequestedOrdering{dmlRequestedOrdering(id, properties.RequestedSortOrderAscending)})

	runDMLOrderingRule(t, NewPushRequestedOrderingThroughInsertRule(), insert,
		insertRef, constraints, true)

	parts := pushedDMLOrdering(t, constraints, inputQ.GetRangesOver()).GetParts()
	if len(parts) != 1 {
		t.Fatalf("pushed parts = %d, want 1", len(parts))
	}
	assertDMLOrderingField(t, parts[0].Value, id)
}

func TestPushRequestedOrderingThroughInsert_NoConstraintDoesNotPush(t *testing.T) {
	t.Parallel()

	inputQ := dmlOrderingQuantifier("insert_input")
	insert := mustDMLOrderingConstruct(expressions.NewInsertExpression(
		inputQ, "MyRecord", dmlOrderingRowType()))
	insertRef := expressions.InitialOf(insert)
	constraints := NewConstraintMap()

	runDMLOrderingRule(t, NewPushRequestedOrderingThroughInsertRule(), insert,
		insertRef, constraints, true)

	if _, ok := Get(constraints, inputQ.GetRangesOver(), RequestedOrderingConstraintKey); ok {
		t.Fatal("constraint should not be pushed when parent has no constraint")
	}
}

func TestPushRequestedOrderingThroughInsert_NotConstraintOnlyDoesNotPush(t *testing.T) {
	t.Parallel()

	inputQ := dmlOrderingQuantifier("insert_input")
	insert := mustDMLOrderingConstruct(expressions.NewInsertExpression(
		inputQ, "MyRecord", dmlOrderingRowType()))
	insertRef := expressions.InitialOf(insert)
	constraints := NewConstraintMap()
	Set(constraints, insertRef, RequestedOrderingConstraintKey,
		[]*properties.RequestedOrdering{dmlRequestedOrdering(
			dmlOrderingField(inputQ, 0), properties.RequestedSortOrderAscending)})

	runDMLOrderingRule(t, NewPushRequestedOrderingThroughInsertRule(), insert,
		insertRef, constraints, false)

	if _, ok := Get(constraints, inputQ.GetRangesOver(), RequestedOrderingConstraintKey); ok {
		t.Fatal("constraint should not be pushed during implementation pass")
	}
}

func TestPushRequestedOrderingThroughInsert_NoYield(t *testing.T) {
	t.Parallel()

	inputQ := dmlOrderingQuantifier("insert_input")
	insert := mustDMLOrderingConstruct(expressions.NewInsertExpression(
		inputQ, "MyRecord", dmlOrderingRowType()))
	insertRef := expressions.InitialOf(insert)
	constraints := NewConstraintMap()
	Set(constraints, insertRef, RequestedOrderingConstraintKey,
		[]*properties.RequestedOrdering{dmlRequestedOrdering(
			dmlOrderingField(inputQ, 0), properties.RequestedSortOrderAscending)})

	call := runDMLOrderingRule(t, NewPushRequestedOrderingThroughInsertRule(), insert,
		insertRef, constraints, true)
	if len(call.yielded) != 0 {
		t.Fatalf("constraint-push rule yielded %d expressions, want 0", len(call.yielded))
	}
}

// --- Update ---

func dmlOrderingUpdate(inputQ expressions.Quantifier) *expressions.UpdateExpression {
	return mustDMLOrderingConstruct(expressions.NewUpdateExpression(
		inputQ,
		"MyRecord",
		dmlOrderingRowType(),
		[]expressions.UpdateTransform{{
			FieldPath: "NAME",
			NewValue: &values.ConstantValue{
				Value: "updated",
				Typ:   values.NotNullString,
			},
		}},
	))
}

func TestPushRequestedOrderingThroughUpdate_PropagatesConstraint(t *testing.T) {
	t.Parallel()

	inputQ := dmlOrderingQuantifier("update_input")
	update := dmlOrderingUpdate(inputQ)
	updateRef := expressions.InitialOf(update)
	// UPDATE emits {OLD,NEW}. An order requested on OLD.ID must be translated
	// through that exact two-level result, not blindly copied as an output path.
	oldID := dmlOrderingOutputField(inputQ.GetAlias(), update.GetResultValue(), 0, 0)
	inputID := dmlOrderingField(inputQ, 0)
	constraints := NewConstraintMap()
	Set(constraints, updateRef, RequestedOrderingConstraintKey,
		[]*properties.RequestedOrdering{dmlRequestedOrdering(
			oldID, properties.RequestedSortOrderDescending)})

	runDMLOrderingRule(t, NewPushRequestedOrderingThroughUpdateRule(), update,
		updateRef, constraints, true)

	parts := pushedDMLOrdering(t, constraints, inputQ.GetRangesOver()).GetParts()
	if len(parts) != 1 {
		t.Fatalf("pushed parts = %d, want 1", len(parts))
	}
	assertDMLOrderingField(t, parts[0].Value, inputID)
	if parts[0].SortOrder != properties.RequestedSortOrderDescending {
		t.Fatalf("sort order = %v, want DESC", parts[0].SortOrder)
	}
}

func TestPushRequestedOrderingThroughUpdate_NewRecordOrderingBecomesPreserve(t *testing.T) {
	t.Parallel()

	inputQ := dmlOrderingQuantifier("update_input")
	update := dmlOrderingUpdate(inputQ)
	updateRef := expressions.InitialOf(update)
	newID := dmlOrderingOutputField(inputQ.GetAlias(), update.GetResultValue(), 1, 0)
	constraints := NewConstraintMap()
	Set(constraints, updateRef, RequestedOrderingConstraintKey,
		[]*properties.RequestedOrdering{dmlRequestedOrdering(
			newID, properties.RequestedSortOrderAscending)})

	runDMLOrderingRule(t, NewPushRequestedOrderingThroughUpdateRule(), update,
		updateRef, constraints, true)

	if pushed := pushedDMLOrdering(t, constraints, inputQ.GetRangesOver()); !pushed.IsPreserve() {
		t.Fatalf("NEW-record ordering pushed as %#v, want preserve", pushed.GetParts())
	}
}

func TestPushRequestedOrderingThroughUpdate_NoConstraintDoesNotPush(t *testing.T) {
	t.Parallel()

	inputQ := dmlOrderingQuantifier("update_input")
	update := dmlOrderingUpdate(inputQ)
	updateRef := expressions.InitialOf(update)
	constraints := NewConstraintMap()

	runDMLOrderingRule(t, NewPushRequestedOrderingThroughUpdateRule(), update,
		updateRef, constraints, true)

	if _, ok := Get(constraints, inputQ.GetRangesOver(), RequestedOrderingConstraintKey); ok {
		t.Fatal("constraint should not be pushed when parent has no constraint")
	}
}

func TestPushRequestedOrderingThroughUpdate_NotConstraintOnlyDoesNotPush(t *testing.T) {
	t.Parallel()

	inputQ := dmlOrderingQuantifier("update_input")
	update := dmlOrderingUpdate(inputQ)
	updateRef := expressions.InitialOf(update)
	constraints := NewConstraintMap()
	Set(constraints, updateRef, RequestedOrderingConstraintKey,
		[]*properties.RequestedOrdering{dmlRequestedOrdering(
			dmlOrderingOutputField(inputQ.GetAlias(), update.GetResultValue(), 0, 0),
			properties.RequestedSortOrderAscending)})

	runDMLOrderingRule(t, NewPushRequestedOrderingThroughUpdateRule(), update,
		updateRef, constraints, false)

	if _, ok := Get(constraints, inputQ.GetRangesOver(), RequestedOrderingConstraintKey); ok {
		t.Fatal("constraint should not be pushed during implementation pass")
	}
}

func TestPushRequestedOrderingThroughUpdate_NoYield(t *testing.T) {
	t.Parallel()

	inputQ := dmlOrderingQuantifier("update_input")
	update := dmlOrderingUpdate(inputQ)
	updateRef := expressions.InitialOf(update)
	constraints := NewConstraintMap()
	Set(constraints, updateRef, RequestedOrderingConstraintKey,
		[]*properties.RequestedOrdering{dmlRequestedOrdering(
			dmlOrderingOutputField(inputQ.GetAlias(), update.GetResultValue(), 0, 0),
			properties.RequestedSortOrderAscending)})

	call := runDMLOrderingRule(t, NewPushRequestedOrderingThroughUpdateRule(), update,
		updateRef, constraints, true)
	if len(call.yielded) != 0 {
		t.Fatalf("constraint-push rule yielded %d expressions, want 0", len(call.yielded))
	}
}

// --- TempTableInsert ---

func dmlOrderingTempTableInsert(
	inputQ expressions.Quantifier,
	owning bool,
) *expressions.TempTableInsertExpression {
	return mustDMLOrderingConstruct(expressions.NewTempTableInsertExpression(
		inputQ, values.NamedCorrelationIdentifier("tt1"), owning))
}

func TestPushRequestedOrderingThroughTempTableInsert_PropagatesConstraint(t *testing.T) {
	t.Parallel()

	inputQ := dmlOrderingQuantifier("temp_insert_input")
	tempInsert := dmlOrderingTempTableInsert(inputQ, true)
	tempInsertRef := expressions.InitialOf(tempInsert)
	col1 := dmlOrderingField(inputQ, 2)
	constraints := NewConstraintMap()
	Set(constraints, tempInsertRef, RequestedOrderingConstraintKey,
		[]*properties.RequestedOrdering{dmlRequestedOrdering(
			col1, properties.RequestedSortOrderAscending)})

	runDMLOrderingRule(t, NewPushRequestedOrderingThroughTempTableInsertRule(),
		tempInsert, tempInsertRef, constraints, true)

	parts := pushedDMLOrdering(t, constraints, inputQ.GetRangesOver()).GetParts()
	if len(parts) != 1 {
		t.Fatalf("pushed parts = %d, want 1", len(parts))
	}
	assertDMLOrderingField(t, parts[0].Value, col1)
}

func TestPushRequestedOrderingThroughTempTableInsert_NoConstraintDoesNotPush(t *testing.T) {
	t.Parallel()

	inputQ := dmlOrderingQuantifier("temp_insert_input")
	tempInsert := dmlOrderingTempTableInsert(inputQ, false)
	tempInsertRef := expressions.InitialOf(tempInsert)
	constraints := NewConstraintMap()

	runDMLOrderingRule(t, NewPushRequestedOrderingThroughTempTableInsertRule(),
		tempInsert, tempInsertRef, constraints, true)

	if _, ok := Get(constraints, inputQ.GetRangesOver(), RequestedOrderingConstraintKey); ok {
		t.Fatal("constraint should not be pushed when parent has no constraint")
	}
}

func TestPushRequestedOrderingThroughTempTableInsert_NotConstraintOnlyDoesNotPush(t *testing.T) {
	t.Parallel()

	inputQ := dmlOrderingQuantifier("temp_insert_input")
	tempInsert := dmlOrderingTempTableInsert(inputQ, true)
	tempInsertRef := expressions.InitialOf(tempInsert)
	constraints := NewConstraintMap()
	Set(constraints, tempInsertRef, RequestedOrderingConstraintKey,
		[]*properties.RequestedOrdering{dmlRequestedOrdering(
			dmlOrderingField(inputQ, 2), properties.RequestedSortOrderAscending)})

	runDMLOrderingRule(t, NewPushRequestedOrderingThroughTempTableInsertRule(),
		tempInsert, tempInsertRef, constraints, false)

	if _, ok := Get(constraints, inputQ.GetRangesOver(), RequestedOrderingConstraintKey); ok {
		t.Fatal("constraint should not be pushed during implementation pass")
	}
}

func TestPushRequestedOrderingThroughTempTableInsert_NoYield(t *testing.T) {
	t.Parallel()

	inputQ := dmlOrderingQuantifier("temp_insert_input")
	tempInsert := dmlOrderingTempTableInsert(inputQ, true)
	tempInsertRef := expressions.InitialOf(tempInsert)
	constraints := NewConstraintMap()
	Set(constraints, tempInsertRef, RequestedOrderingConstraintKey,
		[]*properties.RequestedOrdering{dmlRequestedOrdering(
			dmlOrderingField(inputQ, 2), properties.RequestedSortOrderAscending)})

	call := runDMLOrderingRule(t, NewPushRequestedOrderingThroughTempTableInsertRule(),
		tempInsert, tempInsertRef, constraints, true)
	if len(call.yielded) != 0 {
		t.Fatalf("constraint-push rule yielded %d expressions, want 0", len(call.yielded))
	}
}
