package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// TestImplementTypeFilterRule_FiresAfterScanImplemented pins the
// LogicalTypeFilterExpression implementation as Java's ImplementTypeFilterRule
// does it: a scan whose record types the filter already covers is yielded as
// it is, with no type filter; a scan producing a type the filter drops gets a
// TypeFilterPlan keeping the types it shares with the filter.
func TestImplementTypeFilterRule_FiresAfterScanImplemented(t *testing.T) {
	t.Parallel()
	implement := func(scanTypes, filterTypes []string) []expressions.RelationalExpression {
		scan := mustSmallImplementConstruct(expressions.NewFullUnorderedScanExpression(scanTypes, smallImplementRowType()))
		innerRef := expressions.InitialOf(scan)
		tf := mustSmallImplementConstruct(expressions.NewLogicalTypeFilterExpression(
			filterTypes,
			expressions.ForEachQuantifier(innerRef),
		))
		fireSmallImplementRule(t, NewPrimaryScanRule(), innerRef)
		return fireSmallImplementRule(t, NewImplementTypeFilterRule(), expressions.InitialOf(tf))
	}

	covered := implement([]string{"Order"}, []string{"Order"})
	if len(covered) != 1 {
		t.Fatalf("covered: ImplementTypeFilterRule yielded %d, want 1", len(covered))
	}
	if _, ok := covered[0].(*plans.RecordQueryScanPlan); !ok {
		t.Fatalf("covered: yield = %T, want the scan itself (no type filter needed)", covered[0])
	}

	filtered := implement([]string{"Customer", "Order"}, []string{"Order"})
	if len(filtered) != 1 {
		t.Fatalf("filtered: ImplementTypeFilterRule yielded %d, want 1", len(filtered))
	}
	plan, ok := filtered[0].(*plans.RecordQueryTypeFilterPlan)
	if !ok {
		t.Fatalf("filtered: yield = %T, want *plans.RecordQueryTypeFilterPlan", filtered[0])
	}
	rts := plan.GetRecordTypes()
	if len(rts) != 1 || rts[0] != "Order" {
		t.Fatalf("filtered: record types = %v, want [Order]", rts)
	}
	if _, ok := plan.GetInner().(*plans.RecordQueryScanPlan); !ok {
		t.Fatalf("filtered: inner = %T, want *RecordQueryScanPlan", plan.GetInner())
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
