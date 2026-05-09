package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// Delete tests in rule_push_requested_ordering_through_delete_test.go
// (PLANNING-phase constraint propagation).

// --- Insert ---

func TestPushRequestedOrderingThroughInsert_PropagatesConstraint(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	ins := expressions.NewInsertExpression(scanQ, "MyRecord", values.UnknownType)
	insRef := expressions.InitialOf(ins)

	cm := NewConstraintMap()
	ordering := NewRequestedOrdering(
		[]RequestedOrderingPart{
			{Value: &values.FieldValue{Field: "id", Typ: values.UnknownType}, SortOrder: RequestedSortOrderAscending},
		},
		DistinctnessNotDistinct, false,
	)
	Set(cm, insRef, RequestedOrderingConstraintKey, []*RequestedOrdering{ordering})

	rule := NewPushRequestedOrderingThroughInsertRule()
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), ins)
	if len(bindings) != 1 {
		t.Fatalf("matcher should match InsertExpression, got %d bindings", len(bindings))
	}

	call := &ImplementationRuleCall{
		Bindings:       bindings[0],
		Reference:      insRef,
		Constraints:    cm,
		constraintOnly: true,
	}
	rule.OnMatch(call)

	innerRef := ins.GetInner().GetRangesOver()
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
	if !ok || fv.Field != "id" {
		t.Fatalf("expected ordering on id, got %v", parts[0].Value)
	}
}

func TestPushRequestedOrderingThroughInsert_NoConstraintDoesNotPush(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	ins := expressions.NewInsertExpression(scanQ, "MyRecord", values.UnknownType)
	insRef := expressions.InitialOf(ins)

	cm := NewConstraintMap()

	rule := NewPushRequestedOrderingThroughInsertRule()
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), ins)
	call := &ImplementationRuleCall{
		Bindings:       bindings[0],
		Reference:      insRef,
		Constraints:    cm,
		constraintOnly: true,
	}
	rule.OnMatch(call)

	innerRef := ins.GetInner().GetRangesOver()
	_, ok := Get(cm, innerRef, RequestedOrderingConstraintKey)
	if ok {
		t.Fatal("constraint should not be pushed when parent has no constraint")
	}
}

func TestPushRequestedOrderingThroughInsert_NotConstraintOnlyDoesNotPush(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	ins := expressions.NewInsertExpression(scanQ, "MyRecord", values.UnknownType)
	insRef := expressions.InitialOf(ins)

	cm := NewConstraintMap()
	ordering := NewRequestedOrdering(
		[]RequestedOrderingPart{
			{Value: &values.FieldValue{Field: "id", Typ: values.UnknownType}, SortOrder: RequestedSortOrderAscending},
		},
		DistinctnessNotDistinct, false,
	)
	Set(cm, insRef, RequestedOrderingConstraintKey, []*RequestedOrdering{ordering})

	rule := NewPushRequestedOrderingThroughInsertRule()
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), ins)
	call := &ImplementationRuleCall{
		Bindings:       bindings[0],
		Reference:      insRef,
		Constraints:    cm,
		constraintOnly: false,
	}
	rule.OnMatch(call)

	innerRef := ins.GetInner().GetRangesOver()
	_, ok := Get(cm, innerRef, RequestedOrderingConstraintKey)
	if ok {
		t.Fatal("constraint should not be pushed during implementation pass")
	}
}

func TestPushRequestedOrderingThroughInsert_NoYield(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	ins := expressions.NewInsertExpression(scanQ, "MyRecord", values.UnknownType)
	insRef := expressions.InitialOf(ins)

	cm := NewConstraintMap()
	ordering := NewRequestedOrdering(
		[]RequestedOrderingPart{
			{Value: &values.FieldValue{Field: "id", Typ: values.UnknownType}, SortOrder: RequestedSortOrderAscending},
		},
		DistinctnessNotDistinct, false,
	)
	Set(cm, insRef, RequestedOrderingConstraintKey, []*RequestedOrdering{ordering})

	rule := NewPushRequestedOrderingThroughInsertRule()
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), ins)
	call := &ImplementationRuleCall{
		Bindings:       bindings[0],
		Reference:      insRef,
		Constraints:    cm,
		constraintOnly: true,
	}
	rule.OnMatch(call)

	if len(call.yielded) != 0 {
		t.Fatalf("constraint-push rule should not yield expressions, but yielded %d", len(call.yielded))
	}
}

// --- Update ---

func TestPushRequestedOrderingThroughUpdate_PropagatesConstraint(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	transforms := []expressions.UpdateTransform{
		{FieldPath: "name", NewValue: values.LiteralValue("updated")},
	}
	upd := expressions.NewUpdateExpression(scanQ, "MyRecord", transforms)
	updRef := expressions.InitialOf(upd)

	cm := NewConstraintMap()
	ordering := NewRequestedOrdering(
		[]RequestedOrderingPart{
			{Value: &values.FieldValue{Field: "id", Typ: values.UnknownType}, SortOrder: RequestedSortOrderDescending},
		},
		DistinctnessNotDistinct, false,
	)
	Set(cm, updRef, RequestedOrderingConstraintKey, []*RequestedOrdering{ordering})

	rule := NewPushRequestedOrderingThroughUpdateRule()
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), upd)
	if len(bindings) != 1 {
		t.Fatalf("matcher should match UpdateExpression, got %d bindings", len(bindings))
	}

	call := &ImplementationRuleCall{
		Bindings:       bindings[0],
		Reference:      updRef,
		Constraints:    cm,
		constraintOnly: true,
	}
	rule.OnMatch(call)

	innerRef := upd.GetInner().GetRangesOver()
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
	if !ok || fv.Field != "id" {
		t.Fatalf("expected ordering on id, got %v", parts[0].Value)
	}
	if parts[0].SortOrder != RequestedSortOrderDescending {
		t.Fatalf("expected DESC, got %v", parts[0].SortOrder)
	}
}

func TestPushRequestedOrderingThroughUpdate_NoConstraintDoesNotPush(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	upd := expressions.NewUpdateExpression(scanQ, "MyRecord", nil)
	updRef := expressions.InitialOf(upd)

	cm := NewConstraintMap()

	rule := NewPushRequestedOrderingThroughUpdateRule()
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), upd)
	call := &ImplementationRuleCall{
		Bindings:       bindings[0],
		Reference:      updRef,
		Constraints:    cm,
		constraintOnly: true,
	}
	rule.OnMatch(call)

	innerRef := upd.GetInner().GetRangesOver()
	_, ok := Get(cm, innerRef, RequestedOrderingConstraintKey)
	if ok {
		t.Fatal("constraint should not be pushed when parent has no constraint")
	}
}

func TestPushRequestedOrderingThroughUpdate_NotConstraintOnlyDoesNotPush(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	upd := expressions.NewUpdateExpression(scanQ, "MyRecord", nil)
	updRef := expressions.InitialOf(upd)

	cm := NewConstraintMap()
	ordering := NewRequestedOrdering(
		[]RequestedOrderingPart{
			{Value: &values.FieldValue{Field: "id", Typ: values.UnknownType}, SortOrder: RequestedSortOrderAscending},
		},
		DistinctnessNotDistinct, false,
	)
	Set(cm, updRef, RequestedOrderingConstraintKey, []*RequestedOrdering{ordering})

	rule := NewPushRequestedOrderingThroughUpdateRule()
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), upd)
	call := &ImplementationRuleCall{
		Bindings:       bindings[0],
		Reference:      updRef,
		Constraints:    cm,
		constraintOnly: false,
	}
	rule.OnMatch(call)

	innerRef := upd.GetInner().GetRangesOver()
	_, ok := Get(cm, innerRef, RequestedOrderingConstraintKey)
	if ok {
		t.Fatal("constraint should not be pushed during implementation pass")
	}
}

func TestPushRequestedOrderingThroughUpdate_NoYield(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	upd := expressions.NewUpdateExpression(scanQ, "MyRecord", nil)
	updRef := expressions.InitialOf(upd)

	cm := NewConstraintMap()
	ordering := NewRequestedOrdering(
		[]RequestedOrderingPart{
			{Value: &values.FieldValue{Field: "id", Typ: values.UnknownType}, SortOrder: RequestedSortOrderAscending},
		},
		DistinctnessNotDistinct, false,
	)
	Set(cm, updRef, RequestedOrderingConstraintKey, []*RequestedOrdering{ordering})

	rule := NewPushRequestedOrderingThroughUpdateRule()
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), upd)
	call := &ImplementationRuleCall{
		Bindings:       bindings[0],
		Reference:      updRef,
		Constraints:    cm,
		constraintOnly: true,
	}
	rule.OnMatch(call)

	if len(call.yielded) != 0 {
		t.Fatalf("constraint-push rule should not yield expressions, but yielded %d", len(call.yielded))
	}
}

// --- TempTableInsert ---

func TestPushRequestedOrderingThroughTempTableInsert_PropagatesConstraint(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	alias := values.NamedCorrelationIdentifier("tt1")
	tti := expressions.NewTempTableInsertExpression(scanQ, alias, true)
	ttiRef := expressions.InitialOf(tti)

	cm := NewConstraintMap()
	ordering := NewRequestedOrdering(
		[]RequestedOrderingPart{
			{Value: &values.FieldValue{Field: "col1", Typ: values.UnknownType}, SortOrder: RequestedSortOrderAscending},
		},
		DistinctnessNotDistinct, false,
	)
	Set(cm, ttiRef, RequestedOrderingConstraintKey, []*RequestedOrdering{ordering})

	rule := NewPushRequestedOrderingThroughTempTableInsertRule()
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), tti)
	if len(bindings) != 1 {
		t.Fatalf("matcher should match TempTableInsertExpression, got %d bindings", len(bindings))
	}

	call := &ImplementationRuleCall{
		Bindings:       bindings[0],
		Reference:      ttiRef,
		Constraints:    cm,
		constraintOnly: true,
	}
	rule.OnMatch(call)

	innerRef := tti.GetInner().GetRangesOver()
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
}

func TestPushRequestedOrderingThroughTempTableInsert_NoConstraintDoesNotPush(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	alias := values.NamedCorrelationIdentifier("tt1")
	tti := expressions.NewTempTableInsertExpression(scanQ, alias, false)
	ttiRef := expressions.InitialOf(tti)

	cm := NewConstraintMap()

	rule := NewPushRequestedOrderingThroughTempTableInsertRule()
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), tti)
	call := &ImplementationRuleCall{
		Bindings:       bindings[0],
		Reference:      ttiRef,
		Constraints:    cm,
		constraintOnly: true,
	}
	rule.OnMatch(call)

	innerRef := tti.GetInner().GetRangesOver()
	_, ok := Get(cm, innerRef, RequestedOrderingConstraintKey)
	if ok {
		t.Fatal("constraint should not be pushed when parent has no constraint")
	}
}

func TestPushRequestedOrderingThroughTempTableInsert_NotConstraintOnlyDoesNotPush(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	alias := values.NamedCorrelationIdentifier("tt1")
	tti := expressions.NewTempTableInsertExpression(scanQ, alias, true)
	ttiRef := expressions.InitialOf(tti)

	cm := NewConstraintMap()
	ordering := NewRequestedOrdering(
		[]RequestedOrderingPart{
			{Value: &values.FieldValue{Field: "col1", Typ: values.UnknownType}, SortOrder: RequestedSortOrderAscending},
		},
		DistinctnessNotDistinct, false,
	)
	Set(cm, ttiRef, RequestedOrderingConstraintKey, []*RequestedOrdering{ordering})

	rule := NewPushRequestedOrderingThroughTempTableInsertRule()
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), tti)
	call := &ImplementationRuleCall{
		Bindings:       bindings[0],
		Reference:      ttiRef,
		Constraints:    cm,
		constraintOnly: false,
	}
	rule.OnMatch(call)

	innerRef := tti.GetInner().GetRangesOver()
	_, ok := Get(cm, innerRef, RequestedOrderingConstraintKey)
	if ok {
		t.Fatal("constraint should not be pushed during implementation pass")
	}
}

func TestPushRequestedOrderingThroughTempTableInsert_NoYield(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	alias := values.NamedCorrelationIdentifier("tt1")
	tti := expressions.NewTempTableInsertExpression(scanQ, alias, true)
	ttiRef := expressions.InitialOf(tti)

	cm := NewConstraintMap()
	ordering := NewRequestedOrdering(
		[]RequestedOrderingPart{
			{Value: &values.FieldValue{Field: "col1", Typ: values.UnknownType}, SortOrder: RequestedSortOrderAscending},
		},
		DistinctnessNotDistinct, false,
	)
	Set(cm, ttiRef, RequestedOrderingConstraintKey, []*RequestedOrdering{ordering})

	rule := NewPushRequestedOrderingThroughTempTableInsertRule()
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), tti)
	call := &ImplementationRuleCall{
		Bindings:       bindings[0],
		Reference:      ttiRef,
		Constraints:    cm,
		constraintOnly: true,
	}
	rule.OnMatch(call)

	if len(call.yielded) != 0 {
		t.Fatalf("constraint-push rule should not yield expressions, but yielded %d", len(call.yielded))
	}
}
