package plans

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func indexOrderingLayout() *values.RecordType {
	return values.NewRecordType("index_ordering_row", false, []values.Field{
		{Name: "A", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "B", FieldType: values.NotNullLong, Ordinal: 1},
		{Name: "C", FieldType: values.NotNullLong, Ordinal: 2},
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 3},
	})
}

// TestRecordQueryIndexPlan_HintOrdering_Unbound pins the pre-existing
// unbound-scan behavior: with no scan comparisons, every index key column is
// a sorted key (in scan direction), followed by the trimmed PK suffix.
func TestRecordQueryIndexPlan_HintOrdering_Unbound(t *testing.T) {
	t.Parallel()
	plan := mustChecked(t, func() (*RecordQueryIndexPlan, error) {
		return NewRecordQueryIndexPlan("IDX", nil, []string{"T"}, indexOrderingLayout(), false)
	}).
		WithKeyComponentTypes(testPhysicalLongTypes(2)).
		WithIndexMetadata([]string{"A", "B"}, []string{"ID"}, false).
		WithPrimaryKeyComponentTypes(testPhysicalLongTypes(1))

	got := plan.HintOrdering()
	if !got.IsKnown || len(got.Keys) != 3 {
		t.Fatalf("HintOrdering(unbound) = %#v, want [A, B, ID]", got)
	}
	wantFields := []string{"A", "B", "ID"}
	for i, w := range wantFields {
		fv, ok := values.AsFieldValue(got.Keys[i])
		if !ok || fv.DisplayName() != w {
			t.Fatalf("HintOrdering(unbound).Keys[%d] = %#v, want field %q", i, got.Keys[i], w)
		}
	}
	if got.DescendingAt(0) || got.DescendingAt(1) || got.DescendingAt(2) {
		t.Fatalf("HintOrdering(unbound) = %#v, want ascending", got)
	}
}

// TestRecordQueryIndexPlan_HintOrdering_EqualityPrefixDropped is the
// index-scan analog of TestPKScanOrdering_EqualityPrefixDropped above: a
// leading equality-bound index-key column must NOT appear in the returned
// ordering Keys — same firstNonEq loop as PKScanOrdering (ordering.go),
// just walking index columnNames instead of PK Values. Before this pin,
// nothing in the plans package unit-tested RecordQueryIndexPlan's own
// firstNonEq loop directly (see index_scan_ordering_partition_test.go in the
// cascades package for the end-to-end partitioning proof).
func TestRecordQueryIndexPlan_HintOrdering_EqualityPrefixDropped(t *testing.T) {
	t.Parallel()
	plan := mustChecked(t, func() (*RecordQueryIndexPlan, error) {
		return NewRecordQueryIndexPlan("IDX", nil, []string{"T"}, indexOrderingLayout(), false)
	}).
		WithScanComparisons([]*predicates.ComparisonRange{pkOrderingEq(t, int64(7))}).
		WithKeyComponentTypes(testPhysicalLongTypes(2)).
		WithIndexMetadata([]string{"A", "B"}, []string{"ID"}, false).
		WithPrimaryKeyComponentTypes(testPhysicalLongTypes(1))

	got := plan.HintOrdering()
	if !got.IsKnown || len(got.Keys) != 2 {
		t.Fatalf("HintOrdering(A=7) = %#v, want [B, ID] (A dropped as equality-bound)", got)
	}
	wantFields := []string{"B", "ID"}
	for i, w := range wantFields {
		fv, ok := values.AsFieldValue(got.Keys[i])
		if !ok || fv.DisplayName() != w {
			t.Fatalf("HintOrdering(A=7).Keys[%d] = %#v, want field %q", i, got.Keys[i], w)
		}
	}
}

// TestRecordQueryIndexPlan_HintOrdering_NonEqualityLeadingComparisonKeepsFullKey
// pins that a non-equality leading comparison does NOT trim anything — only
// a genuine equality prefix consumes a sort position.
func TestRecordQueryIndexPlan_HintOrdering_NonEqualityLeadingComparisonKeepsFullKey(t *testing.T) {
	t.Parallel()
	plan := mustChecked(t, func() (*RecordQueryIndexPlan, error) {
		return NewRecordQueryIndexPlan("IDX", nil, []string{"T"}, indexOrderingLayout(), false)
	}).
		WithScanComparisons([]*predicates.ComparisonRange{pkOrderingGT(t, int64(7))}).
		WithKeyComponentTypes(testPhysicalLongTypes(2)).
		WithIndexMetadata([]string{"A", "B"}, []string{"ID"}, false).
		WithPrimaryKeyComponentTypes(testPhysicalLongTypes(1))

	got := plan.HintOrdering()
	if !got.IsKnown || len(got.Keys) != 3 {
		t.Fatalf("HintOrdering(A>7) = %#v, want [A, B, ID] (no equality prefix to drop)", got)
	}
}

// TestRecordQueryIndexPlan_HintOrdering_EqualityPrefixThenRangeStopsAtFirstNonEquality
// pins the three-column boundary case: A=? (equality), B>? (range) — only A
// is dropped; B, C and the trimmed PK suffix stay.
func TestRecordQueryIndexPlan_HintOrdering_EqualityPrefixThenRangeStopsAtFirstNonEquality(t *testing.T) {
	t.Parallel()
	plan := mustChecked(t, func() (*RecordQueryIndexPlan, error) {
		return NewRecordQueryIndexPlan("IDX", nil, []string{"T"}, indexOrderingLayout(), false)
	}).
		WithScanComparisons([]*predicates.ComparisonRange{
			pkOrderingEq(t, int64(7)),
			pkOrderingGT(t, int64(3)),
		}).
		WithKeyComponentTypes(testPhysicalLongTypes(3)).
		WithIndexMetadata([]string{"A", "B", "C"}, []string{"ID"}, false).
		WithPrimaryKeyComponentTypes(testPhysicalLongTypes(1))

	got := plan.HintOrdering()
	if !got.IsKnown || len(got.Keys) != 3 {
		t.Fatalf("HintOrdering(A=7,B>3) = %#v, want [B, C, ID] (only A dropped)", got)
	}
	wantFields := []string{"B", "C", "ID"}
	for i, w := range wantFields {
		fv, ok := values.AsFieldValue(got.Keys[i])
		if !ok || fv.DisplayName() != w {
			t.Fatalf("HintOrdering(A=7,B>3).Keys[%d] = %#v, want field %q", i, got.Keys[i], w)
		}
	}
}
