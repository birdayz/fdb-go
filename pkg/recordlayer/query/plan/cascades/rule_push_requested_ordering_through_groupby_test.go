package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
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
	reqOrd := properties.NewRequestedOrdering([]properties.RequestedOrderingPart{
		{Value: &values.FieldValue{Field: "a", Typ: values.UnknownType}, SortOrder: properties.RequestedSortOrderAscending},
		{Value: &values.FieldValue{Field: "b", Typ: values.UnknownType}, SortOrder: properties.RequestedSortOrderDescending},
	}, properties.DistinctnessNotDistinct, false)
	Set(cm, gbRef, RequestedOrderingConstraintKey, []*properties.RequestedOrdering{reqOrd})

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
	if fv := parts[0].Value.(*values.FieldValue); fv.Field != "a" || parts[0].SortOrder != properties.RequestedSortOrderAscending {
		t.Fatalf("first part: want a ASC, got %s %v", fv.Field, parts[0].SortOrder)
	}
	if fv := parts[1].Value.(*values.FieldValue); fv.Field != "b" || parts[1].SortOrder != properties.RequestedSortOrderDescending {
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
	reqOrd := properties.NewRequestedOrdering([]properties.RequestedOrderingPart{
		{Value: &values.FieldValue{Field: "a", Typ: values.UnknownType}, SortOrder: properties.RequestedSortOrderAscending},
	}, properties.DistinctnessNotDistinct, false)
	Set(cm, gbRef, RequestedOrderingConstraintKey, []*properties.RequestedOrdering{reqOrd})

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
	if fv := parts[0].Value.(*values.FieldValue); fv.Field != "a" || parts[0].SortOrder != properties.RequestedSortOrderAscending {
		t.Fatalf("first part: want a ASC, got %s %v", fv.Field, parts[0].SortOrder)
	}
	// Second: b ANY (appended remaining)
	if fv := parts[1].Value.(*values.FieldValue); fv.Field != "b" || parts[1].SortOrder != properties.RequestedSortOrderAny {
		t.Fatalf("second part: want b ANY, got %s %v", fv.Field, parts[1].SortOrder)
	}
	// Third: c ANY (appended remaining)
	if fv := parts[2].Value.(*values.FieldValue); fv.Field != "c" || parts[2].SortOrder != properties.RequestedSortOrderAny {
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
	reqOrd := properties.NewRequestedOrdering([]properties.RequestedOrderingPart{
		{Value: &values.FieldValue{Field: "x", Typ: values.UnknownType}, SortOrder: properties.RequestedSortOrderAscending},
	}, properties.DistinctnessNotDistinct, false)
	Set(cm, gbRef, RequestedOrderingConstraintKey, []*properties.RequestedOrdering{reqOrd})

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
	reqOrd := properties.NewRequestedOrdering([]properties.RequestedOrderingPart{
		{Value: &values.FieldValue{Field: "a", Typ: values.UnknownType}, SortOrder: properties.RequestedSortOrderAscending},
	}, properties.DistinctnessNotDistinct, false)
	Set(cm, gbRef, RequestedOrderingConstraintKey, []*properties.RequestedOrdering{reqOrd})

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
	reqOrd := properties.NewRequestedOrdering([]properties.RequestedOrderingPart{
		{Value: &values.FieldValue{Field: "COL1", Typ: values.UnknownType}, SortOrder: properties.RequestedSortOrderAscending},
	}, properties.DistinctnessNotDistinct, false)
	Set(cm, gbRef, RequestedOrderingConstraintKey, []*properties.RequestedOrdering{reqOrd})

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
	reqOrd := properties.NewRequestedOrdering([]properties.RequestedOrderingPart{
		{Value: &values.FieldValue{Field: "x", Typ: values.UnknownType}, SortOrder: properties.RequestedSortOrderAscending},
	}, properties.DistinctnessNotDistinct, false)
	Set(cm, gbRef, RequestedOrderingConstraintKey, []*properties.RequestedOrdering{reqOrd})

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
	reqOrd := properties.NewRequestedOrdering([]properties.RequestedOrderingPart{
		{Value: &values.FieldValue{Field: "a", Typ: values.UnknownType}, SortOrder: properties.RequestedSortOrderAscending},
	}, properties.DistinctnessNotDistinct, false)
	Set(cm, gbRef, RequestedOrderingConstraintKey, []*properties.RequestedOrdering{reqOrd})

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

// TestPushRequestedOrderingThroughGroupBy_PushesTheGroupingKeyNotTheRequest pins
// the TRANSLATION at the quantifier boundary.
//
// An ORDER BY above a GROUP BY addresses the aggregate's OUTPUT row; the child
// below this quantifier speaks the INPUT row. Java makes that explicit —
// PushRequestedOrderingThroughGroupByRule.java:108-110 pushes the requested
// ordering down through the group-by's result value before it compares anything
// ("we need to do that in any case"), then :152-153 matches the PUSHED part
// against the grouping values by Value equality — so the part Java pushes IS the
// grouping value.
//
// Go used to push the REQUESTED part through unchanged, having matched it by
// name. Two rows, one spelling, and the reconciliation deferred to whichever
// comparison sat at the access path. It survived only because the candidate side
// there was a bare display name too; the moment either side states an ordinal,
// the pushed request addresses a slot of the wrong row.
//
// The requested value here is an aggregate-OUTPUT reference (slot 0 of
// [K, SUM(V)]) and the grouping key is an INPUT reference (K over T). They are
// the same column and different Values, which is exactly the distinction the
// assertion needs.
func TestPushRequestedOrderingThroughGroupBy_PushesTheGroupingKeyNotTheRequest(t *testing.T) {
	t.Parallel()

	inputRow := values.NewRecordType("", false, []values.Field{
		{Name: "K", FieldType: values.NullableLong, Ordinal: 0},
		{Name: "V", FieldType: values.NullableLong, Ordinal: 1},
	})
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, inputRow)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	groupingKey := values.NewFieldValueWithResolvedOrdinalInDomain(
		"K", 0, values.NullableLong, values.OrdinalDomainOfType(inputRow))
	aggs := []expressions.AggregateSpec{
		{Function: expressions.AggSum, Operand: &values.FieldValue{Field: "V", Typ: values.UnknownType}},
	}
	gb := expressions.NewGroupByExpression([]values.Value{groupingKey}, aggs, scanQ)
	gbRef := expressions.InitialOf(gb)

	// The requested key as the translator hands it over: the aggregate's OUTPUT
	// row, whose single naming authority is GroupByOutputColumnNames.
	outputDomain := values.OrdinalDomainOfColumnNames(
		expressions.GroupByOutputColumnNames([]values.Value{groupingKey}, aggs))
	requestedValue := values.NewFieldValueWithResolvedOrdinalInDomain(
		"K", 0, values.UnknownType, outputDomain)
	if outputDomain == values.OrdinalDomainOfType(inputRow) {
		t.Fatal("test setup: the aggregate output row and the input row must be " +
			"DIFFERENT layouts, or there is no translation to assert")
	}

	cm := NewConstraintMap()
	Set(cm, gbRef, RequestedOrderingConstraintKey, []*properties.RequestedOrdering{
		properties.NewRequestedOrdering([]properties.RequestedOrderingPart{
			{Value: requestedValue, SortOrder: properties.RequestedSortOrderDescending},
		}, properties.DistinctnessNotDistinct, false),
	})

	rule := NewPushRequestedOrderingThroughGroupByRule()
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), gb)
	if len(bindings) != 1 {
		t.Fatalf("matcher bindings = %d, want 1", len(bindings))
	}
	rule.OnMatch(&ImplementationRuleCall{
		Bindings:       bindings[0],
		Reference:      gbRef,
		Constraints:    cm,
		constraintOnly: true,
	})

	pushed, ok := Get(cm, gb.GetInner().GetRangesOver(), RequestedOrderingConstraintKey)
	if !ok || len(pushed) != 1 {
		t.Fatalf("pushed orderings = %v ok=%v, want exactly 1", pushed, ok)
	}
	parts := pushed[0].GetParts()
	if len(parts) != 1 {
		t.Fatalf("pushed parts = %d, want 1", len(parts))
	}
	if parts[0].SortOrder != properties.RequestedSortOrderDescending {
		t.Fatalf("pushed sort order = %v, want the REQUEST's DESC — the "+
			"direction comes from above, only the Value is translated",
			parts[0].SortOrder)
	}
	// Compared by IDENTITY, not by ValuesStructurallyEqual. The untranslated
	// requested value and the grouping key render identically AND compare
	// structurally equal — FieldPath.Equals is ordinal-only and does not compare
	// the layout — so a structural assertion here passes with the translation
	// missing. That is not a hypothetical: it is what this test did on its first
	// writing, staying green while the corpus plan-shape golden went red.
	pushedIdent, pushedOK := values.OrderingIdentityOf(parts[0].Value)
	keyIdent, keyOK := values.OrderingIdentityOf(groupingKey)
	if !keyOK {
		t.Fatal("test setup: the grouping key must state an identity")
	}
	if !pushedOK || pushedIdent != keyIdent {
		t.Fatalf("pushed value = %q (identity %+v ok=%v), want the GROUPING KEY "+
			"%q (identity %+v).\n\n"+
			"What was pushed is the requested value, which addresses the "+
			"AGGREGATE OUTPUT row — not the input row the child below "+
			"this quantifier speaks. An access path underneath then has to "+
			"reconcile a key from one row against a key from another, which is "+
			"possible only by their spelling; and once both sides state an "+
			"ordinal, that spelling bridge is what an ordinal-across-layouts "+
			"conflation hides behind.\n\n"+
			"Java translates first and matches second "+
			"(PushRequestedOrderingThroughGroupByRule.java:108-110, :152-153).",
			values.ExplainValue(parts[0].Value), pushedIdent, pushedOK,
			values.ExplainValue(groupingKey), keyIdent)
	}
}
