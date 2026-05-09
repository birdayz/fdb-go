package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// TestImplementTypeFilterRule_FiresAfterScanImplemented pins the
// LogicalTypeFilterExpression → TypeFilterPlan implementation chain.
func TestImplementTypeFilterRule_FiresAfterScanImplemented(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	innerRef := expressions.InitialOf(scan)
	tf := expressions.NewLogicalTypeFilterExpression(
		[]string{"Order"},
		expressions.ForEachQuantifier(innerRef),
	)
	topRef := expressions.InitialOf(tf)

	FireExpressionRule(NewPrimaryScanRule(), innerRef)

	yielded := FireExpressionRule(NewImplementTypeFilterRule(), topRef)
	if len(yielded) != 1 {
		t.Fatalf("ImplementTypeFilterRule yielded %d, want 1", len(yielded))
	}
	wrap, ok := yielded[0].(*physicalTypeFilterWrapper)
	if !ok {
		t.Fatalf("yield = %T, want *physicalTypeFilterWrapper", yielded[0])
	}
	plan := wrap.GetPlan()
	rts := plan.GetRecordTypes()
	if len(rts) != 1 || rts[0] != "Order" {
		t.Fatalf("record types = %v, want [Order]", rts)
	}
	if _, ok := plan.GetInner().(*plans.RecordQueryScanPlan); !ok {
		t.Fatalf("inner = %T, want *RecordQueryScanPlan", plan.GetInner())
	}
}

// TestImplementTypeFilterRule_NoFireWithoutPhysicalInner pins the
// gate.
func TestImplementTypeFilterRule_NoFireWithoutPhysicalInner(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	tf := expressions.NewLogicalTypeFilterExpression(
		[]string{"Order"},
		expressions.ForEachQuantifier(expressions.InitialOf(scan)),
	)
	topRef := expressions.InitialOf(tf)

	yielded := FireExpressionRule(NewImplementTypeFilterRule(), topRef)
	if len(yielded) != 0 {
		t.Fatalf("ImplementTypeFilterRule fired without physical inner; yielded %d", len(yielded))
	}
}
