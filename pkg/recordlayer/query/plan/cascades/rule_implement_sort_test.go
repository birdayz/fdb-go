package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func TestMakeStrictlySorted_PreservesPhysicalFetchEdges(t *testing.T) {
	t.Parallel()
	for _, covering := range []bool{false, true} {
		name := "index"
		if covering {
			name = "covering"
		}
		t.Run(name, func(t *testing.T) {
			index := mustSortRuleConstruct(plans.NewRecordQueryIndexPlan("idx", nil, []string{"T"}, sortRuleRowType(), false))
			var inner plans.RecordQueryPlan = index
			if covering {
				inner = mustSortRuleConstruct(plans.NewRecordQueryCoveringIndexPlan(index))
			}
			fetch := mustSortRuleConstruct(plans.NewRecordQueryFetchFromPartialRecordPlan(inner, nil, sortRuleRowType(), plans.FetchIndexRecordsSyntheticConstituents))
			result, err := makeStrictlySorted(fetch)
			if err != nil {
				t.Fatal(err)
			}
			assertProducerPhysicalQuantifiers(t, result)
			rebuilt := result.(*plans.RecordQueryFetchFromPartialRecordPlan)
			if rebuilt.GetFetchIndexRecords() != fetch.GetFetchIndexRecords() {
				t.Fatal("fetch copy lost its fetch mode")
			}
			copiedIndex, ok := plans.IndexPlanOf(rebuilt.GetInner())
			if !ok || !copiedIndex.IsStrictlySorted() || index.IsStrictlySorted() {
				t.Fatal("strict sorting did not copy the selected index")
			}
		})
	}
}

func sortRuleRowType() *values.RecordType {
	return values.NewRecordType("SortRuleRow", false, []values.Field{
		{Name: "id", FieldType: values.NotNullLong},
		{Name: "name", FieldType: values.NullableString},
		{Name: "a", FieldType: values.NullableLong},
	})
}

func mustSortRuleConstruct[T any](value T, err error) T {
	if err != nil {
		panic("construct sort-rule fixture: " + err.Error())
	}
	return value
}

func sortRuleScanAndQOV() (*expressions.FullUnorderedScanExpression, values.QuantifiedObjectValue) {
	scan := mustSortRuleConstruct(expressions.NewFullUnorderedScanExpression(
		[]string{"T"}, sortRuleRowType()))
	q := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	return scan, mustSortRuleConstruct(q.RequireFlowedObjectValue())
}

func TestImplementSortRule_GetRequestedOrderings(t *testing.T) {
	t.Parallel()
	scan, root := sortRuleScanAndQOV()
	keys := []expressions.SortKey{
		{Value: mustSortRuleConstruct(values.ResolveFieldOrdinals(root, []int{0})), Reverse: false},
		{Value: mustSortRuleConstruct(values.ResolveFieldOrdinals(root, []int{1})), Reverse: true},
	}
	sort := mustSortRuleConstruct(expressions.NewLogicalSortExpression(
		keys, expressions.ForEachQuantifier(expressions.InitialOf(scan))))

	rule := NewImplementSortRule()
	orderings := rule.GetRequestedOrderings(sort)
	if len(orderings) != 1 {
		t.Fatalf("expected 1 ordering, got %d", len(orderings))
	}
	parts := orderings[0].GetParts()
	if len(parts) != 2 {
		t.Fatalf("expected 2 parts, got %d", len(parts))
	}
	if parts[0].SortOrder != properties.RequestedSortOrderAscending {
		t.Fatal("first key should be ascending")
	}
	if parts[1].SortOrder != properties.RequestedSortOrderDescending {
		t.Fatal("second key should be descending")
	}
}

func TestImplementSortRule_PreserveOrdering(t *testing.T) {
	t.Parallel()
	scan, _ := sortRuleScanAndQOV()
	sort := mustSortRuleConstruct(expressions.NewLogicalSortExpression(
		nil, expressions.ForEachQuantifier(expressions.InitialOf(scan))))

	rule := NewImplementSortRule()
	orderings := rule.GetRequestedOrderings(sort)
	if len(orderings) != 1 {
		t.Fatalf("expected 1 ordering, got %d", len(orderings))
	}
	if !orderings[0].IsPreserve() {
		t.Fatal("empty sort keys should produce preserve ordering")
	}
}

func TestSortExpressionToRequestedOrdering(t *testing.T) {
	t.Parallel()
	scan, root := sortRuleScanAndQOV()
	keys := []expressions.SortKey{
		{Value: mustSortRuleConstruct(values.ResolveFieldOrdinals(root, []int{2})), Reverse: false},
	}
	sort := mustSortRuleConstruct(expressions.NewLogicalSortExpression(
		keys, expressions.ForEachQuantifier(expressions.InitialOf(scan))))

	req := sortExpressionToRequestedOrdering(sort)
	if req.Size() != 1 {
		t.Fatalf("expected 1 part, got %d", req.Size())
	}
	if req.GetParts()[0].SortOrder != properties.RequestedSortOrderAscending {
		t.Fatal("expected ascending")
	}
}
