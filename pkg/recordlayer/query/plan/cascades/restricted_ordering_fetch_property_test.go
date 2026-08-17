package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func restrictedOrderingFetchRowType() *values.RecordType {
	return values.NewRecordType("RestrictedOrderingFetchRow", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong},
	})
}

func restrictedOrderingFetchScan(t testing.TB) *plans.RecordQueryScanPlan {
	t.Helper()
	scan, err := plans.NewRecordQueryScanPlan(
		[]string{"RestrictedOrderingFetchRow"}, restrictedOrderingFetchRowType(), false)
	if err != nil {
		t.Fatalf("construct exact scan: %v", err)
	}
	return scan
}

// TestMemoizeFinalExpressionsFromOtherPreservesRequestedOrdering pins the
// constraint carried by a partition-restricted child reference. The DML
// stored-record filter legitimately admits Fetch(Covering(Index)); without
// this copy, optimizing the fresh restricted reference forgets why that
// ordered alternative was retained and can replace it with an unordered one.
//
// MUTATION: remove the RequestedOrderingConstraintKey copy in
// MemoizeFinalExpressionsFromOther. The restricted reference then has no
// ordering constraint and this test fails before plan extraction can silently
// discard the ordered partition.
func TestMemoizeFinalExpressionsFromOtherPreservesRequestedOrdering(t *testing.T) {
	t.Parallel()

	scan := restrictedOrderingFetchScan(t)
	orderingValue, err := values.ResolveFieldOrdinals(scan.GetResultValue(), []int{0})
	if err != nil {
		t.Fatalf("resolve exact ordering field: %v", err)
	}
	requested := properties.NewRequestedOrdering(
		[]properties.RequestedOrderingPart{{
			Value:     orderingValue,
			SortOrder: properties.RequestedSortOrderAscending,
		}},
		properties.DistinctnessNotDistinct,
		false,
	)

	source := expressions.InitialOf(scan)
	constraints := NewConstraintMap()
	if !Set(constraints, source, RequestedOrderingConstraintKey, []*properties.RequestedOrdering{requested}) {
		t.Fatal("initial requested-ordering constraint did not grow the constraint lattice")
	}

	call := &ImplementationRuleCall{Constraints: constraints}
	restricted := call.MemoizeFinalExpressionsFromOther(
		source, []expressions.RelationalExpression{scan})

	got, ok := Get(constraints, restricted, RequestedOrderingConstraintKey)
	if !ok {
		t.Fatal("restricted final reference lost its source requested-ordering constraint")
	}
	if len(got) != 1 || got[0] != requested {
		t.Fatalf("restricted requested orderings = %#v, want the exact source requirement %#v", got, requested)
	}
}

// TestFetchFromPartialRecordPublishesStoredRecord pins the Java property
// contract that Fetch turns a partial index entry into a stored record. DML
// candidate selection filters on this property; delegating to the covering
// child discards the only composite-index Fetch plan capable of applying an IN
// predicate before the fetch.
//
// MUTATION: remove RecordQueryFetchFromPartialRecordPlan from
// computeStoredRecord's true arm. This test fails and the equivalent SELECT
// and DML statements choose different access paths again.
func TestFetchFromPartialRecordPublishesStoredRecord(t *testing.T) {
	t.Parallel()

	scan := restrictedOrderingFetchScan(t)
	fetch, err := plans.NewRecordQueryFetchFromPartialRecordPlan(
		scan,
		plans.UnableToTranslate,
		restrictedOrderingFetchRowType(),
		plans.FetchIndexRecordsPrimaryKey,
	)
	if err != nil {
		t.Fatalf("construct exact fetch: %v", err)
	}
	if !computeStoredRecord(fetch) {
		t.Fatal("FetchFromPartialRecord emits a stored record but the property reports false")
	}
	if !computeWrapperProperties(fetch).GetBool(properties.PropStoredRecord) {
		t.Fatal("wrapper properties did not publish FetchFromPartialRecord's stored-record result")
	}
}
