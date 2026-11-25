package rowdiff

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// rowdiffPlanFixtureType is the exact row shape used by the synthetic cost and
// ordering plans. It mirrors the relevant columns of genTable: ID and B are
// NOT NULL, while A is nullable. Constructors snapshot this type, so every
// fixture carries a stable result Value and ordinal layout instead of relying
// on the retired nil/unknown-type compatibility path.
func rowdiffPlanFixtureType() values.Type {
	return values.NewRecordType("rowdiff_plan_fixture", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "A", FieldType: values.NullableLong, Ordinal: 1},
		{Name: "B", FieldType: values.NotNullLong, Ordinal: 2},
	})
}

func mustRowdiffPlan[T any](t testing.TB, result T, err error) T {
	t.Helper()
	if err != nil {
		t.Fatalf("construct rowdiff plan fixture: %v", err)
	}
	return result
}

func rowdiffTestScan(t testing.TB, reverse bool) *plans.RecordQueryScanPlan {
	t.Helper()
	scan, err := plans.NewRecordQueryScanPlan(
		[]string{"T"}, rowdiffPlanFixtureType(), reverse)
	return mustRowdiffPlan(t, scan, err)
}

func rowdiffTestSort(
	t testing.TB,
	inner plans.RecordQueryPlan,
	keys []plans.SortKey,
) *plans.RecordQueryInMemorySortPlan {
	t.Helper()
	bakedKeys := rowdiffTestSortKeys(t, inner.GetResultValue(), keys)
	sort, err := plans.NewRecordQueryInMemorySortPlan(inner, bakedKeys)
	return mustRowdiffPlan(t, sort, err)
}

func rowdiffTestSortKeys(
	t testing.TB,
	owner values.Value,
	keys []plans.SortKey,
) []plans.SortKey {
	t.Helper()
	bakedKeys := append([]plans.SortKey(nil), keys...)
	for i := range bakedKeys {
		if bakedKeys[i].ValueExpr != nil {
			continue
		}
		ordinal, _ := rowdiffColumn(t, bakedKeys[i].Field)
		value, err := values.ResolveFieldOrdinals(owner, []int{ordinal})
		bakedKeys[i].ValueExpr = mustRowdiffPlan(t, value, err)
	}
	return bakedKeys
}

func rowdiffTestCovering(
	t testing.TB,
	index *plans.RecordQueryIndexPlan,
) *plans.RecordQueryCoveringIndexPlan {
	t.Helper()
	covering, err := plans.NewRecordQueryCoveringIndexPlan(index)
	return mustRowdiffPlan(t, covering, err)
}

func rowdiffTestFetch(
	t testing.TB,
	inner plans.RecordQueryPlan,
) *plans.RecordQueryFetchFromPartialRecordPlan {
	t.Helper()
	fetch, err := plans.NewRecordQueryFetchFromPartialRecordPlan(
		inner, nil, rowdiffPlanFixtureType(), plans.FetchIndexRecordsPrimaryKey)
	return mustRowdiffPlan(t, fetch, err)
}

func rowdiffTestUnorderedUnion(
	t testing.TB,
	inners []plans.RecordQueryPlan,
) *plans.RecordQueryUnorderedUnionPlan {
	t.Helper()
	union, err := plans.NewRecordQueryUnorderedUnionPlan(inners)
	return mustRowdiffPlan(t, union, err)
}

func rowdiffColumnTypes(t testing.TB, columns []string) []values.Type {
	t.Helper()
	types := make([]values.Type, len(columns))
	for i, column := range columns {
		_, types[i] = rowdiffColumn(t, column)
	}
	return types
}

func rowdiffColumn(t testing.TB, column string) (int, values.Type) {
	t.Helper()
	switch column {
	case "ID":
		return 0, values.NotNullLong
	case "A":
		return 1, values.NullableLong
	case "B":
		return 2, values.NotNullLong
	default:
		// A fixture adding a new column must state both its exact ordinal and
		// physical type instead of silently receiving UNKNOWN.
		t.Fatalf("rowdiff plan fixture has no exact type for column %q", column)
		return 0, nil
	}
}
