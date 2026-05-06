package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// TestImplementInsertRule_FiresAfterScanImplemented pins that
// InsertExpression over a scan with a physical-implemented inner
// yields a RecordQueryInsertPlan wrapping the inner ScanPlan.
func TestImplementInsertRule_FiresAfterScanImplemented(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	innerRef := expressions.InitialOf(scan)
	ins := expressions.NewInsertExpression(
		expressions.ForEachQuantifier(innerRef),
		"Order",
		values.UnknownType,
	)
	topRef := expressions.InitialOf(ins)

	FireExpressionRule(NewPrimaryScanRule(), innerRef)

	yielded := FireExpressionRule(NewImplementInsertRule(), topRef)
	if len(yielded) != 1 {
		t.Fatalf("ImplementInsertRule yielded %d, want 1", len(yielded))
	}
	wrap, ok := yielded[0].(*physicalInsertWrapper)
	if !ok {
		t.Fatalf("yield = %T, want *physicalInsertWrapper", yielded[0])
	}
	plan := wrap.GetPlan()
	if got := plan.GetTargetRecordType(); got != "Order" {
		t.Fatalf("target = %q, want %q", got, "Order")
	}
	if _, ok := plan.GetInner().(*plans.RecordQueryScanPlan); !ok {
		t.Fatalf("inner = %T, want *RecordQueryScanPlan", plan.GetInner())
	}
}

// TestImplementInsertRule_NoFireWithoutPhysicalInner pins that the
// rule waits if the inner Reference has no physical member.
func TestImplementInsertRule_NoFireWithoutPhysicalInner(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	innerRef := expressions.InitialOf(scan)
	ins := expressions.NewInsertExpression(
		expressions.ForEachQuantifier(innerRef),
		"Order",
		values.UnknownType,
	)
	topRef := expressions.InitialOf(ins)

	yielded := FireExpressionRule(NewImplementInsertRule(), topRef)
	if len(yielded) != 0 {
		t.Fatalf("ImplementInsertRule fired without physical inner; yielded %d", len(yielded))
	}
}

// TestImplementDeleteRule_FiresAfterScanImplemented pins the
// DeleteExpression → DeletePlan implementation chain.
func TestImplementDeleteRule_FiresAfterScanImplemented(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	innerRef := expressions.InitialOf(scan)
	del := expressions.NewDeleteExpression(
		expressions.ForEachQuantifier(innerRef),
		"Order",
	)
	topRef := expressions.InitialOf(del)

	FireExpressionRule(NewPrimaryScanRule(), innerRef)

	yielded := FireExpressionRule(NewImplementDeleteRule(), topRef)
	if len(yielded) != 1 {
		t.Fatalf("ImplementDeleteRule yielded %d, want 1", len(yielded))
	}
	wrap, ok := yielded[0].(*physicalDeleteWrapper)
	if !ok {
		t.Fatalf("yield = %T, want *physicalDeleteWrapper", yielded[0])
	}
	plan := wrap.GetPlan()
	if got := plan.GetTargetRecordType(); got != "Order" {
		t.Fatalf("target = %q, want %q", got, "Order")
	}
	if _, ok := plan.GetInner().(*plans.RecordQueryScanPlan); !ok {
		t.Fatalf("inner = %T, want *RecordQueryScanPlan", plan.GetInner())
	}
}

// TestImplementDeleteRule_NoFireWithoutPhysicalInner pins the gate.
func TestImplementDeleteRule_NoFireWithoutPhysicalInner(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	del := expressions.NewDeleteExpression(
		expressions.ForEachQuantifier(expressions.InitialOf(scan)),
		"Order",
	)
	topRef := expressions.InitialOf(del)

	yielded := FireExpressionRule(NewImplementDeleteRule(), topRef)
	if len(yielded) != 0 {
		t.Fatalf("ImplementDeleteRule fired without physical inner; yielded %d", len(yielded))
	}
}

// TestImplementUpdateRule_FiresAfterScanImplemented pins the
// UpdateExpression → UpdatePlan chain. The transforms list passes
// through unchanged — UpdatePlan applies them per-row at execution.
func TestImplementUpdateRule_FiresAfterScanImplemented(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	innerRef := expressions.InitialOf(scan)
	transforms := []expressions.UpdateTransform{
		{FieldPath: "qty", NewValue: values.LiteralValue(int64(0))},
	}
	upd := expressions.NewUpdateExpression(
		expressions.ForEachQuantifier(innerRef),
		"Order",
		transforms,
	)
	topRef := expressions.InitialOf(upd)

	FireExpressionRule(NewPrimaryScanRule(), innerRef)

	yielded := FireExpressionRule(NewImplementUpdateRule(), topRef)
	if len(yielded) != 1 {
		t.Fatalf("ImplementUpdateRule yielded %d, want 1", len(yielded))
	}
	wrap, ok := yielded[0].(*physicalUpdateWrapper)
	if !ok {
		t.Fatalf("yield = %T, want *physicalUpdateWrapper", yielded[0])
	}
	plan := wrap.GetPlan()
	if got := plan.GetTargetRecordType(); got != "Order" {
		t.Fatalf("target = %q, want %q", got, "Order")
	}
	if got := len(plan.GetTransforms()); got != 1 {
		t.Fatalf("transforms = %d, want 1", got)
	}
	if _, ok := plan.GetInner().(*plans.RecordQueryScanPlan); !ok {
		t.Fatalf("inner = %T, want *RecordQueryScanPlan", plan.GetInner())
	}
}

// TestImplementUpdateRule_NoFireWithoutPhysicalInner pins the gate.
func TestImplementUpdateRule_NoFireWithoutPhysicalInner(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	upd := expressions.NewUpdateExpression(
		expressions.ForEachQuantifier(expressions.InitialOf(scan)),
		"Order",
		nil,
	)
	topRef := expressions.InitialOf(upd)

	yielded := FireExpressionRule(NewImplementUpdateRule(), topRef)
	if len(yielded) != 0 {
		t.Fatalf("ImplementUpdateRule fired without physical inner; yielded %d", len(yielded))
	}
}
