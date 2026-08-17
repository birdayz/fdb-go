package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// TestImplementTypeFilterRule_FiresAfterScanImplemented pins the
// LogicalTypeFilterExpression → TypeFilterPlan implementation chain.
func TestImplementTypeFilterRule_FiresAfterScanImplemented(t *testing.T) {
	t.Parallel()
	scan := smallImplementScan("Order")
	innerRef := expressions.InitialOf(scan)
	tf := mustSmallImplementConstruct(expressions.NewLogicalTypeFilterExpression(
		[]string{"Order"},
		expressions.ForEachQuantifier(innerRef),
	))
	topRef := expressions.InitialOf(tf)

	fireSmallImplementRule(t, NewPrimaryScanRule(), innerRef)

	yielded := fireSmallImplementRule(t, NewImplementTypeFilterRule(), topRef)
	if len(yielded) != 1 {
		t.Fatalf("ImplementTypeFilterRule yielded %d, want 1", len(yielded))
	}
	plan, ok := yielded[0].(*plans.RecordQueryTypeFilterPlan)
	if !ok {
		t.Fatalf("yield = %T, want *plans.RecordQueryTypeFilterPlan", yielded[0])
	}
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
	scan := smallImplementScan("Order")
	tf := mustSmallImplementConstruct(expressions.NewLogicalTypeFilterExpression(
		[]string{"Order"},
		expressions.ForEachQuantifier(expressions.InitialOf(scan)),
	))
	topRef := expressions.InitialOf(tf)

	yielded := fireSmallImplementRule(t, NewImplementTypeFilterRule(), topRef)
	if len(yielded) != 0 {
		t.Fatalf("ImplementTypeFilterRule fired without physical inner; yielded %d", len(yielded))
	}
}
