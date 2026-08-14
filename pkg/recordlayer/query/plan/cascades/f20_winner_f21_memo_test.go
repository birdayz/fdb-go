package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// TestGetWinnerForOrdering_DeterministicCheapestAmongSatisfyingMembers (F20)
// pins that among MULTIPLE members whose derived rich orderings satisfy the
// request, getWinnerForOrdering returns the `less`-CHEAPEST — with the
// deterministic plan-hash tie-break, so the selection cannot flip across
// plannings when the comparator ties.
func TestGetWinnerForOrdering_DeterministicCheapestAmongSatisfyingMembers(t *testing.T) {
	t.Parallel()

	rowType := values.NewRecordType("WinnerRow", false, []values.Field{
		{Name: "A", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "B", FieldType: values.NotNullLong, Ordinal: 1},
		{Name: "C", FieldType: values.NotNullLong, Ordinal: 2},
	})
	inner, err := plans.NewRecordQueryScanPlan([]string{"T"}, rowType, false)
	inner = mustConstruct(t, inner, err)
	resolve := func(ordinal int) values.Value {
		t.Helper()
		value, resolveErr := values.ResolveFieldOrdinals(inner.GetResultValue(), []int{ordinal})
		return mustConstruct(t, value, resolveErr)
	}
	a, b, c := resolve(0), resolve(1), resolve(2)
	sortedMember := func(keys ...values.Value) expressions.RelationalExpression {
		t.Helper()
		sortKeys := make([]plans.SortKey, len(keys))
		for i, key := range keys {
			field, ok := values.AsFieldValue(key)
			if !ok {
				t.Fatalf("sort key[%d] = %T, want exact FieldValue", i, key)
			}
			sortKeys[i] = plans.SortKey{
				Field:      field.DisplayName(),
				ValueExpr:  key,
				NullsFirst: true,
			}
		}
		plan, constructErr := plans.NewRecordQueryInMemorySortPlan(inner, sortKeys)
		return mustConstruct(t, plan, constructErr)
	}

	// Two members ordered (A,B) and (A,C): both satisfy a request of (A asc).
	w1 := sortedMember(a, b)
	w2 := sortedMember(a, c)
	ref := expressions.InitialOf(w1)
	ref.Insert(w2)

	reqOrd := properties.NewRequestedOrdering(
		[]properties.RequestedOrderingPart{{Value: a, SortOrder: properties.RequestedSortOrderAscending}},
		properties.DistinctnessPreserveDistinctness, false)

	// A rank map gives an explicit, controllable total order over the two members.
	rankOf := func(cheapest expressions.RelationalExpression) func(a, b expressions.RelationalExpression) bool {
		return func(a, b expressions.RelationalExpression) bool {
			rank := func(e expressions.RelationalExpression) int {
				if e == cheapest {
					return 0
				}
				return 1
			}
			return rank(a) < rank(b)
		}
	}

	for run := 0; run < 20; run++ {
		if got, _ := getWinnerForOrdering(ref, reqOrd, rankOf(w1)); got != w1 {
			t.Fatalf("run %d: cheapest=w1 but got %p (w1=%p w2=%p)", run, got, w1, w2)
		}
		if got, _ := getWinnerForOrdering(ref, reqOrd, rankOf(w2)); got != w2 {
			t.Fatalf("run %d: cheapest=w2 but got %p", run, got)
		}
	}

	// A comparator that ties everything: the plan-hash tie-break must make
	// the selection deterministic across repeated lookups.
	tie := func(a, b expressions.RelationalExpression) bool { return false }
	first, _ := getWinnerForOrdering(ref, reqOrd, tie)
	if first == nil {
		t.Fatal("tied lookup returned nil")
	}
	for run := 0; run < 20; run++ {
		if got, _ := getWinnerForOrdering(ref, reqOrd, tie); got != first {
			t.Fatalf("run %d: tied selection flipped from %p to %p", run, first, got)
		}
	}
}

// eqIndexScan builds an index scan on `idx` with a single equality comparand.
func eqIndexScan(t *testing.T, lit int64) *plans.RecordQueryIndexPlan {
	t.Helper()
	res := predicates.EmptyComparisonRange().Merge(&predicates.Comparison{
		Type:    predicates.ComparisonEquals,
		Operand: &values.ConstantValue{Value: lit, Typ: values.NotNullLong},
	})
	if !res.Ok {
		t.Fatalf("merge failed for = %v", lit)
	}
	rowType := values.NewRecordType("IndexRow", false, []values.Field{
		{Name: "c", FieldType: values.NotNullLong, Ordinal: 0},
	})
	plan, err := plans.NewRecordQueryIndexPlan("idx",
		[]*predicates.ComparisonRange{res.Range}, []string{"T"}, rowType, false)
	return mustConstruct(t, plan, err)
}

// TestMemoizeLeaf_IndexScanComparandNotCollapsed (F21 memo-level) pins that two
// index scans differing only in their equality comparand intern into DISTINCT
// References (the memo leaf dedup keys on HashCodeWithoutChildren +
// EqualsWithoutChildren). Before the F21 fix these collapsed into one Reference,
// so extraction could materialize the wrong-comparand scan.
func TestMemoizeLeaf_IndexScanComparandNotCollapsed(t *testing.T) {
	t.Parallel()

	m := NewMemo(nil)
	w5 := eqIndexScan(t, int64(5)).WithIndexMetadata([]string{"c"}, nil, false)
	w7 := eqIndexScan(t, int64(7)).WithIndexMetadata([]string{"c"}, nil, false)

	ref5 := m.MemoizeExpression(w5)
	ref7 := m.MemoizeExpression(w7)
	if ref5 == ref7 {
		t.Error("IndexScan([= 5]) and IndexScan([= 7]) collapsed into ONE Reference — F21 memo collapse")
	}

	// An identical-comparand scan MUST still intern into the SAME Reference
	// (dedup preserved for genuine twins).
	w5b := eqIndexScan(t, int64(5)).WithIndexMetadata([]string{"c"}, nil, false)
	ref5b := m.MemoizeExpression(w5b)
	if ref5 != ref5b {
		t.Error("identical IndexScan([= 5]) must intern into the SAME Reference")
	}
}
