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

// TestRecordQueryIndexPlan_HintRichOrdering_UntypedOperandOnDoubleIsNotFixed is
// the index-scan twin of the aggregate arm of the same name: a DOUBLE index
// column bound by an UNKNOWN-typed non-constant operand may be zero at runtime
// and widen across both signed-zero blocks, so the rich form binds it SORTED
// (own order, scan direction) and drops the tail; it must never be FIXED, the
// reading the operand-only predicate gives. On a LONG column the same operand
// IS fixed and the PK suffix stays claimable.
func TestRecordQueryIndexPlan_HintRichOrdering_UntypedOperandOnDoubleIsNotFixed(t *testing.T) {
	t.Parallel()
	untyped := plannerDynamicEquality(t, values.UnknownType)
	layout := values.NewRecordType("index_ordering_double_row", false, []values.Field{
		{Name: "D", FieldType: values.NullableDouble, Ordinal: 0},
		{Name: "B", FieldType: values.NotNullLong, Ordinal: 1},
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 2},
	})
	double := mustChecked(t, func() (*RecordQueryIndexPlan, error) {
		return NewRecordQueryIndexPlan("IDX", []*predicates.ComparisonRange{untyped}, []string{"T"}, layout, false)
	}).
		WithKeyComponentTypes([]values.Type{values.NullableDouble, values.NotNullLong}).
		WithIndexMetadata([]string{"D", "B"}, []string{"ID"}, false).
		WithPrimaryKeyComponentTypes(testPhysicalLongTypes(1))
	if got := double.HintOrdering(); got.IsKnown {
		t.Fatalf("HintOrdering(d = ?) = %#v, want unknown", got)
	}
	rich := double.HintRichOrdering()
	if n := len(rich.GetKeys()); n != 1 {
		t.Fatalf("HintRichOrdering(d = ?) has %d keys, want [D] alone with the tail dropped", n)
	}
	if b := rich.GetBindingMap()[rich.GetKeys()[0]]; len(b) != 1 || b[0].IsFixed() {
		t.Fatalf("D bound by an untyped operand = %v, want SORTED, not FIXED", b)
	}

	long := mustChecked(t, func() (*RecordQueryIndexPlan, error) {
		return NewRecordQueryIndexPlan("IDX", []*predicates.ComparisonRange{untyped}, []string{"T"}, indexOrderingLayout(), false)
	}).
		WithKeyComponentTypes(testPhysicalLongTypes(2)).
		WithIndexMetadata([]string{"A", "B"}, []string{"ID"}, false).
		WithPrimaryKeyComponentTypes(testPhysicalLongTypes(1))
	richLong := long.HintRichOrdering()
	if n := len(richLong.GetKeys()); n != 3 {
		t.Fatalf("HintRichOrdering(a = ? on a LONG) has %d keys, want [A, B, ID]", n)
	}
	if b := richLong.GetBindingMap()[richLong.GetKeys()[0]]; len(b) != 1 || !b[0].IsFixed() {
		t.Fatalf("A (LONG) bound by an untyped operand = %v, want FIXED", b)
	}
}

// TestRecordQueryIndexPlan_HintRichOrdering_PinnedCoordinateAfterWidenedOneStaysFixed:
// FIXED is a PER-COORDINATE fact, not a prefix length. Under `d = 0.0 AND
// b = 1` over INDEX(d, b) the signed-zero equality on D widens across two
// physical blocks — so D binds SORTED and the tail (the PK suffix) is dropped —
// but every admitted row still carries b = 1, one physical key within each
// block, so B binds FIXED exactly as it does behind a pinned D. Demoting B to
// SORTED because it sits after a widened coordinate forfeits `ORDER BY b DESC`
// over a forward scan, a plan the operand-only classification at the
// merge-base produced.
func TestRecordQueryIndexPlan_HintRichOrdering_PinnedCoordinateAfterWidenedOneStaysFixed(t *testing.T) {
	t.Parallel()
	layout := values.NewRecordType("index_ordering_double_row", false, []values.Field{
		{Name: "D", FieldType: values.NullableDouble, Ordinal: 0},
		{Name: "B", FieldType: values.NotNullLong, Ordinal: 1},
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 2},
	})
	plan := mustChecked(t, func() (*RecordQueryIndexPlan, error) {
		return NewRecordQueryIndexPlan("IDX",
			[]*predicates.ComparisonRange{pkOrderingEq(t, float64(0)), pkOrderingEq(t, int64(1))},
			[]string{"T"}, layout, false)
	}).
		WithKeyComponentTypes([]values.Type{values.NullableDouble, values.NotNullLong}).
		WithIndexMetadata([]string{"D", "B"}, []string{"ID"}, false).
		WithPrimaryKeyComponentTypes(testPhysicalLongTypes(1))
	if got := plan.HintOrdering(); got.IsKnown {
		t.Fatalf("HintOrdering(d = 0.0, b = 1) = %#v, want unknown: the PK suffix restarts at D's block boundary", got)
	}
	rich := plan.HintRichOrdering()
	if n := len(rich.GetKeys()); n != 2 {
		t.Fatalf("HintRichOrdering(d = 0.0, b = 1) has %d keys, want [D, B] with the PK suffix dropped", n)
	}
	bm := rich.GetBindingMap()
	if b := bm[rich.GetKeys()[0]]; len(b) != 1 || b[0].IsFixed() {
		t.Fatalf("D under a signed-zero equality = %v, want SORTED", b)
	}
	if b := bm[rich.GetKeys()[1]]; len(b) != 1 || !b[0].IsFixed() {
		t.Fatalf("B = 1 after a widened D = %v, want FIXED: every admitted row carries b = 1", b)
	}
	if rich.StorageKeyIsComplete() {
		t.Fatalf("storage-key completeness claimed with the PK suffix dropped")
	}
}
