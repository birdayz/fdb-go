package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func TestOrderedPrimaryScanRule_FiresForPrimaryKeyOrdering(t *testing.T) {
	t.Parallel()

	ctx := &rfc190PrimaryKeyPlanContext{
		primaryKeyColumns: []string{"TENANT_ID", "ID"},
	}
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	sortExpr := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: values.NewFlatFieldValue("TENANT_ID", values.NullableLong)},
			{Value: values.NewFlatFieldValue("ID", values.NullableLong)},
		},
		expressions.ForEachQuantifier(expressions.InitialOf(scan)),
	)

	yielded := FireExpressionRuleWithMemo(
		NewOrderedPrimaryScanRule(),
		expressions.InitialOf(sortExpr),
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
