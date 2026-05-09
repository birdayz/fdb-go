package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// Delete tests moved to rule_push_requested_ordering_through_delete_test.go
// (PLANNING-phase constraint propagation).

// --- Insert ---

func TestPushOrderingThroughInsert_SortKeysPassThrough(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	ins := expressions.NewInsertExpression(scanQ, "MyRecord", values.UnknownType)
	insQ := expressions.ForEachQuantifier(expressions.InitialOf(ins))
	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: &values.FieldValue{Field: "id", Typ: values.UnknownType}, Reverse: true},
		},
		insQ,
	)
	ref := expressions.InitialOf(sort)

	yielded := FireExpressionRule(NewPushOrderingThroughInsertRule(), ref)
	if len(yielded) != 1 {
		t.Fatalf("yielded %d, want 1", len(yielded))
	}

	newIns, ok := yielded[0].(*expressions.InsertExpression)
	if !ok {
		t.Fatalf("expected *InsertExpression, got %T", yielded[0])
	}
	if newIns.GetTargetRecordType() != "MyRecord" {
		t.Fatalf("expected target MyRecord, got %s", newIns.GetTargetRecordType())
	}
	innerSort, ok := newIns.GetInner().GetRangesOver().Get().(*expressions.LogicalSortExpression)
	if !ok {
		t.Fatalf("expected *LogicalSortExpression below Insert, got %T", newIns.GetInner().GetRangesOver().Get())
	}
	if len(innerSort.GetSortKeys()) != 1 {
		t.Fatalf("expected 1 sort key, got %d", len(innerSort.GetSortKeys()))
	}
	if !innerSort.GetSortKeys()[0].Reverse {
		t.Fatal("expected DESC sort key")
	}
}

func TestPushOrderingThroughInsert_UnsortedDoesNotFire(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	ins := expressions.NewInsertExpression(scanQ, "MyRecord", values.UnknownType)
	insQ := expressions.ForEachQuantifier(expressions.InitialOf(ins))
	sort := expressions.UnsortedLogicalSortExpression(insQ)
	ref := expressions.InitialOf(sort)

	yielded := FireExpressionRule(NewPushOrderingThroughInsertRule(), ref)
	if len(yielded) != 0 {
		t.Fatalf("rule should not fire on unsorted, but yielded %d", len(yielded))
	}
}

// --- Update ---

func TestPushOrderingThroughUpdate_SortKeysPassThrough(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	transforms := []expressions.UpdateTransform{
		{FieldPath: "name", NewValue: values.LiteralValue("updated")},
	}
	upd := expressions.NewUpdateExpression(scanQ, "MyRecord", transforms)
	updQ := expressions.ForEachQuantifier(expressions.InitialOf(upd))
	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: &values.FieldValue{Field: "id", Typ: values.UnknownType}, Reverse: false},
		},
		updQ,
	)
	ref := expressions.InitialOf(sort)

	yielded := FireExpressionRule(NewPushOrderingThroughUpdateRule(), ref)
	if len(yielded) != 1 {
		t.Fatalf("yielded %d, want 1", len(yielded))
	}

	newUpd, ok := yielded[0].(*expressions.UpdateExpression)
	if !ok {
		t.Fatalf("expected *UpdateExpression, got %T", yielded[0])
	}
	if newUpd.GetTargetRecordType() != "MyRecord" {
		t.Fatalf("expected target MyRecord, got %s", newUpd.GetTargetRecordType())
	}
	if len(newUpd.GetTransforms()) != 1 {
		t.Fatalf("expected 1 transform, got %d", len(newUpd.GetTransforms()))
	}
	innerSort, ok := newUpd.GetInner().GetRangesOver().Get().(*expressions.LogicalSortExpression)
	if !ok {
		t.Fatalf("expected *LogicalSortExpression below Update, got %T", newUpd.GetInner().GetRangesOver().Get())
	}
	if len(innerSort.GetSortKeys()) != 1 {
		t.Fatalf("expected 1 sort key, got %d", len(innerSort.GetSortKeys()))
	}
}

func TestPushOrderingThroughUpdate_UnsortedDoesNotFire(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	upd := expressions.NewUpdateExpression(scanQ, "MyRecord", nil)
	updQ := expressions.ForEachQuantifier(expressions.InitialOf(upd))
	sort := expressions.UnsortedLogicalSortExpression(updQ)
	ref := expressions.InitialOf(sort)

	yielded := FireExpressionRule(NewPushOrderingThroughUpdateRule(), ref)
	if len(yielded) != 0 {
		t.Fatalf("rule should not fire on unsorted, but yielded %d", len(yielded))
	}
}

// --- TempTableInsert ---

func TestPushOrderingThroughTempTableInsert_SortKeysPassThrough(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	alias := values.NamedCorrelationIdentifier("tt1")
	tti := expressions.NewTempTableInsertExpression(scanQ, alias, true)
	ttiQ := expressions.ForEachQuantifier(expressions.InitialOf(tti))
	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: &values.FieldValue{Field: "col1", Typ: values.UnknownType}, Reverse: false},
		},
		ttiQ,
	)
	ref := expressions.InitialOf(sort)

	yielded := FireExpressionRule(NewPushOrderingThroughTempTableInsertRule(), ref)
	if len(yielded) != 1 {
		t.Fatalf("yielded %d, want 1", len(yielded))
	}

	newTTI, ok := yielded[0].(*expressions.TempTableInsertExpression)
	if !ok {
		t.Fatalf("expected *TempTableInsertExpression, got %T", yielded[0])
	}
	if newTTI.GetTempTableAlias() != alias {
		t.Fatalf("expected alias %v, got %v", alias, newTTI.GetTempTableAlias())
	}
	if !newTTI.IsOwning() {
		t.Fatal("expected owning=true")
	}
	innerSort, ok := newTTI.GetInner().GetRangesOver().Get().(*expressions.LogicalSortExpression)
	if !ok {
		t.Fatalf("expected *LogicalSortExpression below TempTableInsert, got %T", newTTI.GetInner().GetRangesOver().Get())
	}
	if len(innerSort.GetSortKeys()) != 1 {
		t.Fatalf("expected 1 sort key, got %d", len(innerSort.GetSortKeys()))
	}
}

func TestPushOrderingThroughTempTableInsert_UnsortedDoesNotFire(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	alias := values.NamedCorrelationIdentifier("tt1")
	tti := expressions.NewTempTableInsertExpression(scanQ, alias, false)
	ttiQ := expressions.ForEachQuantifier(expressions.InitialOf(tti))
	sort := expressions.UnsortedLogicalSortExpression(ttiQ)
	ref := expressions.InitialOf(sort)

	yielded := FireExpressionRule(NewPushOrderingThroughTempTableInsertRule(), ref)
	if len(yielded) != 0 {
		t.Fatalf("rule should not fire on unsorted, but yielded %d", len(yielded))
	}
}
