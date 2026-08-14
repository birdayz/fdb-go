package plans

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestRecordQueryScanPlan_LeafShape(t *testing.T) {
	t.Parallel()
	p := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"Order"}, exactTestRecordType(), false)
	})
	if got := len(p.GetChildren()); got != 0 {
		t.Fatalf("scan has %d children, want 0", got)
	}
	if rts := p.GetRecordTypes(); len(rts) != 1 || rts[0] != "Order" {
		t.Fatalf("record types = %v, want [Order]", rts)
	}
}

func TestRecordQueryScanPlan_DedupTypes(t *testing.T) {
	t.Parallel()
	// Duplicates collapse via dedupSortedStrings.
	p := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T", "U", "T"}, exactTestRecordType(), false)
	})
	rts := p.GetRecordTypes()
	if len(rts) != 2 || rts[0] != "T" || rts[1] != "U" {
		t.Fatalf("record types = %v, want [T, U]", rts)
	}
}

func TestRecordQueryFilterPlan_WrapsInner(t *testing.T) {
	t.Parallel()
	scan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T"}, exactTestRecordType(), false)
	})
	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	filter := mustChecked(t, func() (*RecordQueryFilterPlan, error) {
		return NewRecordQueryFilterPlan([]predicates.QueryPredicate{pred}, scan)
	})
	cs := filter.GetChildren()
	if len(cs) != 1 || cs[0] != scan {
		t.Fatalf("filter children = %v, want [scan]", cs)
	}
}

func TestEquals_Recursive(t *testing.T) {
	t.Parallel()
	scanA := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T"}, exactTestRecordType(), false)
	})
	scanB := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T"}, exactTestRecordType(), false)
	})
	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	filterA := mustChecked(t, func() (*RecordQueryFilterPlan, error) {
		return NewRecordQueryFilterPlan([]predicates.QueryPredicate{pred}, scanA)
	})
	filterB := mustChecked(t, func() (*RecordQueryFilterPlan, error) {
		return NewRecordQueryFilterPlan([]predicates.QueryPredicate{pred}, scanB)
	})
	if !Equals(filterA, filterB) {
		t.Fatal("structurally-equal filter plans should compare equal")
	}
	scanC := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"U"}, exactTestRecordType(), false)
	})
	filterC := mustChecked(t, func() (*RecordQueryFilterPlan, error) {
		return NewRecordQueryFilterPlan([]predicates.QueryPredicate{pred}, scanC)
	})
	if Equals(filterA, filterC) {
		t.Fatal("filter plans over different scans should NOT be equal")
	}
}

func TestEquals_NilHandling(t *testing.T) {
	t.Parallel()
	if !Equals(nil, nil) {
		t.Fatal("Equals(nil, nil) should be true")
	}
	scan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T"}, exactTestRecordType(), false)
	})
	if Equals(scan, nil) || Equals(nil, scan) {
		t.Fatal("Equals(plan, nil) should be false")
	}
}

func TestSize_CountsAllNodes(t *testing.T) {
	t.Parallel()
	scan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T"}, exactTestRecordType(), false)
	})
	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	filter := mustChecked(t, func() (*RecordQueryFilterPlan, error) {
		return NewRecordQueryFilterPlan([]predicates.QueryPredicate{pred}, scan)
	})

	keys := []SortKey{{Field: "id", ValueExpr: testField(t, "id", values.NullableLong)}}
	sort := mustChecked(t, func() (*RecordQueryInMemorySortPlan, error) {
		return NewRecordQueryInMemorySortPlan(filter, keys)
	})
	if got := Size(sort); got != 3 {
		t.Fatalf("Size(InMemorySort(Filter(Scan))) = %d, want 3", got)
	}
}

func TestExplain_Renders(t *testing.T) {
	t.Parallel()
	scan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"Order"}, exactTestRecordType(), true)
	})
	if got := scan.Explain(); got != "Scan(Order) REVERSE" {
		t.Fatalf("scan Explain = %q", got)
	}
	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	filter := mustChecked(t, func() (*RecordQueryFilterPlan, error) {
		return NewRecordQueryFilterPlan([]predicates.QueryPredicate{pred}, scan)
	})
	got := filter.Explain()
	want := "Filter([1 preds], Scan(Order) REVERSE)"
	if got != want {
		t.Fatalf("filter Explain = %q, want %q", got, want)
	}
}

// ---------------------------------------------------------------------------
// RecordQueryIndexPlan: WithStrictlySorted / IsStrictlySorted
// ---------------------------------------------------------------------------

func TestRecordQueryIndexPlan_StrictlySorted(t *testing.T) {
	t.Parallel()

	orig := mustChecked(t, func() (*RecordQueryIndexPlan, error) {
		return NewRecordQueryIndexPlan("idx_a", nil, []string{"T"}, exactTestRecordType(), false)
	})
	if orig.IsStrictlySorted() {
		t.Fatal("new index plan should not be strictlySorted")
	}

	strict := orig.WithStrictlySorted()
	if !strict.IsStrictlySorted() {
		t.Fatal("WithStrictlySorted plan should be strictlySorted")
	}

	// Original must be unmodified (shallow copy, not mutation).
	if orig.IsStrictlySorted() {
		t.Fatal("original plan must remain non-strictlySorted after WithStrictlySorted")
	}

	// All other fields must be preserved.
	if strict.GetIndexName() != orig.GetIndexName() {
		t.Fatalf("index name = %q, want %q", strict.GetIndexName(), orig.GetIndexName())
	}
	if strict.IsReverse() != orig.IsReverse() {
		t.Fatalf("reverse = %v, want %v", strict.IsReverse(), orig.IsReverse())
	}
	if !orig.GetFlowedType().Equals(strict.GetFlowedType()) {
		t.Fatalf("flowed type changed: got %v, want %v", strict.GetFlowedType(), orig.GetFlowedType())
	}

	// EqualsWithoutChildren distinguishes strictlySorted from non-.
	if Equals(orig, strict) {
		t.Fatal("non-strictlySorted and strictlySorted plans should not be equal")
	}

	// Two strictlySorted copies of the same plan should be equal.
	strict2 := orig.WithStrictlySorted()
	if !Equals(strict, strict2) {
		t.Fatal("two strictlySorted copies of the same plan should be equal")
	}

	// HashCodeWithoutChildren should differ.
	h1 := orig.HashCodeWithoutChildren()
	h2 := strict.HashCodeWithoutChildren()
	if h1 == h2 {
		t.Fatal("hash codes should differ between strictlySorted and non-strictlySorted")
	}
}

func TestRecordQueryIndexPlan_WithStrictlySorted_Reverse(t *testing.T) {
	t.Parallel()

	orig := mustChecked(t, func() (*RecordQueryIndexPlan, error) {
		return NewRecordQueryIndexPlan("idx_b", nil, []string{"T"}, exactTestRecordType(), true)
	})
	strict := orig.WithStrictlySorted()

	if !strict.IsReverse() {
		t.Fatal("reverse flag should be preserved by WithStrictlySorted")
	}
	if !strict.IsStrictlySorted() {
		t.Fatal("should be strictlySorted")
	}
}
