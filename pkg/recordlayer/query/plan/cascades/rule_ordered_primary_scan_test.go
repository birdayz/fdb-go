package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

type orderedPrimaryScanPlanContext struct {
	indexTestPlanContext
	primaryKeyColumns []string
}

func (c *orderedPrimaryScanPlanContext) GetPrimaryKeyColumns(string) []string {
	return append([]string(nil), c.primaryKeyColumns...)
}

func TestOrderedPrimaryScanRule_FiresForPrimaryKeyOrdering(t *testing.T) {
	t.Parallel()

	ctx := &orderedPrimaryScanPlanContext{
		primaryKeyColumns: []string{"TENANT_ID", "ID"},
	}
	// The flowed type must be exact. Sort elision depends on the physical type
	// of each key coordinate, and each sort Value must be rooted in this
	// quantifier rather than a name-only placeholder.
	scan := mustOrderedScanFull(t, []string{"T"})
	scanRef := mustOrderedScanInitial(t, scan)
	quantifier := expressions.ForEachQuantifier(scanRef)
	flowed := mustOrderedScanFlowed(t, quantifier)
	sortExpr := mustOrderedScanSort(t,
		[]expressions.SortKey{
			{Value: mustOrderedScanField(t, flowed, "TENANT_ID")},
			{Value: mustOrderedScanField(t, flowed, "ID")},
		},
		quantifier,
	)

	yielded := mustFireExpressionRuleWithMemo(t,
		NewOrderedPrimaryScanRule(),
		mustOrderedScanInitial(t, sortExpr),
		ctx,
		nil,
	)
	if len(yielded) != 1 {
		t.Fatalf("expected one ordered primary scan, got %d", len(yielded))
	}
	primary, ok := yielded[0].(*plans.RecordQueryScanPlan)
	if !ok {
		t.Fatalf("yielded %T, want *plans.RecordQueryScanPlan", yielded[0])
	}
	if primary.IsReverse() {
		t.Fatal("ascending requested ordering produced a reverse primary scan")
	}
	pk := primary.GetPrimaryKeyValues()
	if len(pk) != 2 ||
		!values.AccessorNamePathMatchesNames(pk[0], []string{"TENANT_ID"}) ||
		!values.AccessorNamePathMatchesNames(pk[1], []string{"ID"}) {
		t.Fatalf("primary-key stamp = %#v, want [TENANT_ID, ID]", pk)
	}
}
