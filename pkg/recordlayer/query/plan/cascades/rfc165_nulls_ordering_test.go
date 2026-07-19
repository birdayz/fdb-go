package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestMatchedSortOrder_IsCounterflowNulls pins the counterflow truth table for
// MatchedSortOrder, which gates AbstractDataAccessRule.satisfiesRequestedOrdering
// (Java AbstractDataAccessRule.java:820): a data-access match must not report a
// counterflow request as satisfied by a natural matched order.
func TestMatchedSortOrder_IsCounterflowNulls(t *testing.T) {
	t.Parallel()
	cases := []struct {
		m    MatchedSortOrder
		want bool
	}{
		{MatchedSortOrderAscending, false},           // ASC_NULLS_FIRST
		{MatchedSortOrderDescending, false},          // DESC_NULLS_LAST
		{MatchedSortOrderAscendingNullsLast, true},   // ASC_NULLS_LAST  (counterflow)
		{MatchedSortOrderDescendingNullsFirst, true}, // DESC_NULLS_FIRST (counterflow)
	}
	for _, c := range cases {
		if got := c.m.IsCounterflowNulls(); got != c.want {
			t.Errorf("%s.IsCounterflowNulls() = %v, want %v", c.m, got, c.want)
		}
	}
}

// TestOrderingPartitionHash_CounterflowDistinct pins a review catch:
// orderingPartitionHash must distinguish a counterflow (ASC NULLS LAST) ordering
// from the natural (ASC NULLS FIRST) ordering of the same column+direction, else
// they collide into one partition on the RollUpPlanPartitions (set-op /
// data-access merge) path and the partition advertises the first-seen ordering —
// re-opening the "counterflow == natural" elision bug one layer up.
func TestOrderingPartitionHash_CounterflowDistinct(t *testing.T) {
	t.Parallel()
	a := fieldVal("a")
	naturalAsc := properties.Ordering{IsKnown: true, Keys: []values.Value{a}, Descending: []bool{false}}
	counterflow := properties.Ordering{IsKnown: true, Keys: []values.Value{a}, Descending: []bool{false}, NullsFirst: []bool{false}}
	if orderingPartitionHash(naturalAsc) == orderingPartitionHash(counterflow) {
		t.Fatal("ASC_NULLS_FIRST and ASC_NULLS_LAST must hash to different partitions")
	}
	// Natural ASC with NullsFirst unset vs explicitly natural (true) must NOT
	// split — both are ASC_NULLS_FIRST, and an index scan (unset) and a natural
	// in-memory sort (explicit true) must share a partition.
	naturalExplicit := properties.Ordering{IsKnown: true, Keys: []values.Value{a}, Descending: []bool{false}, NullsFirst: []bool{true}}
	if orderingPartitionHash(naturalAsc) != orderingPartitionHash(naturalExplicit) {
		t.Fatal("natural ASC (NullsFirst unset) and explicit ASC_NULLS_FIRST must share a partition")
	}
}

// TestProvidedSortOrder_IsCompatibleWithRequestedSortOrder_TruthTable pins the
// 1:1 port of Java OrderingPart.ProvidedSortOrder.isCompatibleWithRequestedSortOrder
// (OrderingPart.java:322-332): ANY/CHOOSE/FIXED short-circuit to compatible;
// otherwise NULL placement (counterflow) AND direction (any-ascending) must agree.
//
// Regression for RFC-165 / TODO "NULLS-ORDER": before the fix the check compared
// only ascending-ness (one-sided), so a forward (ASC_NULLS_FIRST) scan was reported
// compatible with an ASC NULLS LAST (ASC_NULLS_LAST) request and the sort was wrongly
// elided -> NULLs came first instead of last.
func TestProvidedSortOrder_IsCompatibleWithRequestedSortOrder_TruthTable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		provided properties.ProvidedSortOrder
		reqd     properties.RequestedSortOrder
		want     bool
	}{
		// ANY request is always compatible.
		{"asc-first vs any", properties.ProvidedSortOrderAscending, properties.RequestedSortOrderAny, true},
		{"asc-last vs any", properties.ProvidedSortOrderAscendingNullsLast, properties.RequestedSortOrderAny, true},
		// FIXED / CHOOSE provided is always compatible (constant / free).
		{"fixed vs asc-last", properties.ProvidedSortOrderFixed, properties.RequestedSortOrderAscendingNullsLast, true},
		{"choose vs desc-first", properties.ProvidedSortOrderChoose, properties.RequestedSortOrderDescendingNullsFirst, true},
		// Natural ascending: ASC_NULLS_FIRST.
		{"asc-first vs asc", properties.ProvidedSortOrderAscending, properties.RequestedSortOrderAscending, true},
		{"asc-first vs asc-nulls-last", properties.ProvidedSortOrderAscending, properties.RequestedSortOrderAscendingNullsLast, false}, // THE bug
		{"asc-first vs desc", properties.ProvidedSortOrderAscending, properties.RequestedSortOrderDescending, false},
		{"asc-first vs desc-nulls-first", properties.ProvidedSortOrderAscending, properties.RequestedSortOrderDescendingNullsFirst, false},
		// Counterflow ascending: ASC_NULLS_LAST.
		{"asc-last vs asc-nulls-last", properties.ProvidedSortOrderAscendingNullsLast, properties.RequestedSortOrderAscendingNullsLast, true},
		{"asc-last vs asc", properties.ProvidedSortOrderAscendingNullsLast, properties.RequestedSortOrderAscending, false},
		// Natural descending: DESC_NULLS_LAST.
		{"desc-last vs desc", properties.ProvidedSortOrderDescending, properties.RequestedSortOrderDescending, true},
		{"desc-last vs desc-nulls-first", properties.ProvidedSortOrderDescending, properties.RequestedSortOrderDescendingNullsFirst, false}, // symmetric bug
		{"desc-last vs asc", properties.ProvidedSortOrderDescending, properties.RequestedSortOrderAscending, false},
		// Counterflow descending: DESC_NULLS_FIRST.
		{"desc-first vs desc-nulls-first", properties.ProvidedSortOrderDescendingNullsFirst, properties.RequestedSortOrderDescendingNullsFirst, true},
		{"desc-first vs desc", properties.ProvidedSortOrderDescendingNullsFirst, properties.RequestedSortOrderDescending, false},
	}
	for _, c := range cases {
		if got := c.provided.IsCompatibleWithRequestedSortOrder(c.reqd); got != c.want {
			t.Errorf("%s: IsCompatibleWithRequestedSortOrder = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestRichOrdering_Satisfies_CounterflowNulls is the satisfaction-level regression:
// a forward (ASC_NULLS_FIRST) ordering must NOT satisfy an ASC NULLS LAST request
// (so the sort is retained), but must still satisfy a plain ASC request (so the
// legitimate natural-order elision is preserved).
func TestRichOrdering_Satisfies_CounterflowNulls(t *testing.T) {
	t.Parallel()
	a := fieldVal("a")
	o := properties.NewRichOrdering(
		map[values.Value][]properties.OrderingBinding{
			a: {properties.SortedBinding(properties.ProvidedSortOrderAscending)}, // forward scan = ASC_NULLS_FIRST
		},
		[]values.Value{a},
		false,
	)

	ascNullsLast := properties.NewRequestedOrdering([]properties.RequestedOrderingPart{
		{Value: a, SortOrder: properties.RequestedSortOrderAscendingNullsLast},
	}, properties.DistinctnessNotDistinct, false)
	if o.Satisfies(ascNullsLast) {
		t.Fatal("ASC_NULLS_FIRST ordering must NOT satisfy an ASC NULLS LAST request (sort must be retained)")
	}

	ascNatural := properties.NewRequestedOrdering([]properties.RequestedOrderingPart{
		{Value: a, SortOrder: properties.RequestedSortOrderAscending},
	}, properties.DistinctnessNotDistinct, false)
	if !o.Satisfies(ascNatural) {
		t.Fatal("ASC_NULLS_FIRST ordering must still satisfy a plain ASC request (natural-order elision preserved)")
	}
}

// TestEnumerateSatisfyingComparisonKeyValues_RefusesCounterflow pins a review
// must-fix: a counterflow request must not be reported as satisfiable through a
// set-operation comparison key (Go emits a plain Value, not the ToOrderedBytesValue
// physical encoding), so the merge cannot lie about NULL placement.
func TestEnumerateSatisfyingComparisonKeyValues_RefusesCounterflow(t *testing.T) {
	t.Parallel()
	a := fieldVal("a")
	o := properties.NewRichOrdering(
		map[values.Value][]properties.OrderingBinding{
			a: {properties.SortedBinding(properties.ProvidedSortOrderAscending)},
		},
		[]values.Value{a},
		false,
	)
	counterflow := properties.NewRequestedOrdering([]properties.RequestedOrderingPart{
		{Value: a, SortOrder: properties.RequestedSortOrderAscendingNullsLast},
	}, properties.DistinctnessNotDistinct, false)
	if got := o.EnumerateSatisfyingComparisonKeyValues(counterflow); got != nil {
		t.Fatalf("counterflow request must yield no satisfying comparison keys, got %v", got)
	}
	// Sanity: the natural request still enumerates.
	natural := properties.NewRequestedOrdering([]properties.RequestedOrderingPart{
		{Value: a, SortOrder: properties.RequestedSortOrderAscending},
	}, properties.DistinctnessNotDistinct, false)
	if got := o.EnumerateSatisfyingComparisonKeyValues(natural); got == nil {
		t.Fatal("natural ASC request should still enumerate a satisfying comparison key")
	}
}
