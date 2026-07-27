package plans

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestRecordQueryIndexPlan_HintOrdering_Unbound pins the pre-existing
// unbound-scan behavior: with no scan comparisons, every index key column is
// a sorted key (in scan direction), followed by the trimmed PK suffix.
func TestRecordQueryIndexPlan_HintOrdering_Unbound(t *testing.T) {
	t.Parallel()
	plan := NewRecordQueryIndexPlan("IDX", nil, []string{"T"}, values.UnknownType, false).
		WithIndexMetadata([]string{"A", "B"}, []string{"ID"}, false)

	got := plan.HintOrdering()
	if !got.IsKnown || len(got.Keys) != 3 {
		t.Fatalf("HintOrdering(unbound) = %#v, want [A, B, ID]", got)
	}
	wantFields := []string{"A", "B", "ID"}
	for i, w := range wantFields {
		fv, ok := got.Keys[i].(*values.FieldValue)
		if !ok || fv.Field != w {
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
	plan := NewRecordQueryIndexPlan("IDX", nil, []string{"T"}, values.UnknownType, false).
		WithScanComparisons([]*predicates.ComparisonRange{pkOrderingEq(t, int64(7))}).
		WithIndexMetadata([]string{"A", "B"}, []string{"ID"}, false)

	got := plan.HintOrdering()
	if !got.IsKnown || len(got.Keys) != 2 {
		t.Fatalf("HintOrdering(A=7) = %#v, want [B, ID] (A dropped as equality-bound)", got)
	}
	wantFields := []string{"B", "ID"}
	for i, w := range wantFields {
		fv, ok := got.Keys[i].(*values.FieldValue)
		if !ok || fv.Field != w {
			t.Fatalf("HintOrdering(A=7).Keys[%d] = %#v, want field %q", i, got.Keys[i], w)
		}
	}
}

// TestRecordQueryIndexPlan_HintOrdering_NonEqualityLeadingComparisonKeepsFullKey
// pins that a non-equality leading comparison does NOT trim anything — only
// a genuine equality prefix consumes a sort position.
func TestRecordQueryIndexPlan_HintOrdering_NonEqualityLeadingComparisonKeepsFullKey(t *testing.T) {
	t.Parallel()
	plan := NewRecordQueryIndexPlan("IDX", nil, []string{"T"}, values.UnknownType, false).
		WithScanComparisons([]*predicates.ComparisonRange{pkOrderingGT(t, int64(7))}).
		WithIndexMetadata([]string{"A", "B"}, []string{"ID"}, false)

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
	plan := NewRecordQueryIndexPlan("IDX", nil, []string{"T"}, values.UnknownType, false).
		WithScanComparisons([]*predicates.ComparisonRange{
			pkOrderingEq(t, int64(7)),
			pkOrderingGT(t, int64(3)),
		}).
		WithIndexMetadata([]string{"A", "B", "C"}, []string{"ID"}, false)

	got := plan.HintOrdering()
	if !got.IsKnown || len(got.Keys) != 3 {
		t.Fatalf("HintOrdering(A=7,B>3) = %#v, want [B, C, ID] (only A dropped)", got)
	}
	wantFields := []string{"B", "C", "ID"}
	for i, w := range wantFields {
		fv, ok := got.Keys[i].(*values.FieldValue)
		if !ok || fv.Field != w {
			t.Fatalf("HintOrdering(A=7,B>3).Keys[%d] = %#v, want field %q", i, got.Keys[i], w)
		}
	}
}
