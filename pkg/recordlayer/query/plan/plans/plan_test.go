package plans

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

func TestRecordQueryScanPlan_LeafShape(t *testing.T) {
	t.Parallel()
	p := NewRecordQueryScanPlan([]string{"Order"}, values.UnknownType, false)
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
	p := NewRecordQueryScanPlan([]string{"T", "U", "T"}, values.UnknownType, false)
	rts := p.GetRecordTypes()
	if len(rts) != 2 || rts[0] != "T" || rts[1] != "U" {
		t.Fatalf("record types = %v, want [T, U]", rts)
	}
}

func TestRecordQueryFilterPlan_WrapsInner(t *testing.T) {
	t.Parallel()
	scan := NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	filter := NewRecordQueryFilterPlan([]predicates.QueryPredicate{pred}, scan)
	cs := filter.GetChildren()
	if len(cs) != 1 || cs[0] != scan {
		t.Fatalf("filter children = %v, want [scan]", cs)
	}
}

func TestEquals_Recursive(t *testing.T) {
	t.Parallel()
	scanA := NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	scanB := NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	filterA := NewRecordQueryFilterPlan([]predicates.QueryPredicate{pred}, scanA)
	filterB := NewRecordQueryFilterPlan([]predicates.QueryPredicate{pred}, scanB)
	if !Equals(filterA, filterB) {
		t.Fatal("structurally-equal filter plans should compare equal")
	}
	scanC := NewRecordQueryScanPlan([]string{"U"}, values.UnknownType, false)
	filterC := NewRecordQueryFilterPlan([]predicates.QueryPredicate{pred}, scanC)
	if Equals(filterA, filterC) {
		t.Fatal("filter plans over different scans should NOT be equal")
	}
}

func TestEquals_NilHandling(t *testing.T) {
	t.Parallel()
	if !Equals(nil, nil) {
		t.Fatal("Equals(nil, nil) should be true")
	}
	scan := NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	if Equals(scan, nil) || Equals(nil, scan) {
		t.Fatal("Equals(plan, nil) should be false")
	}
}

func TestSize_CountsAllNodes(t *testing.T) {
	t.Parallel()
	scan := NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	filter := NewRecordQueryFilterPlan([]predicates.QueryPredicate{pred}, scan)

	keys := []expressions.SortKey{{Value: &values.FieldValue{Field: "id", Typ: values.UnknownType}}}
	sort := NewRecordQuerySortPlan(keys, filter)
	if got := Size(sort); got != 3 {
		t.Fatalf("Size(Sort(Filter(Scan))) = %d, want 3", got)
	}
}

func TestRecordQuerySortPlan_PreservesInnerType(t *testing.T) {
	t.Parallel()
	scan := NewRecordQueryScanPlan([]string{"T"}, values.NotNullLong, false)
	keys := []expressions.SortKey{{Value: &values.FieldValue{Field: "id", Typ: values.UnknownType}}}
	sort := NewRecordQuerySortPlan(keys, scan)
	if !values.NotNullLong.Equals(sort.GetResultType()) {
		t.Fatalf("sort result type=%v, want %v", sort.GetResultType(), values.NotNullLong)
	}
}

func TestExplain_Renders(t *testing.T) {
	t.Parallel()
	scan := NewRecordQueryScanPlan([]string{"Order"}, values.UnknownType, true)
	if got := scan.Explain(); got != "Scan(Order) REVERSE" {
		t.Fatalf("scan Explain = %q", got)
	}
	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	filter := NewRecordQueryFilterPlan([]predicates.QueryPredicate{pred}, scan)
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

	orig := NewRecordQueryIndexPlan("idx_a", nil, []string{"T"}, values.UnknownType, false)
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
	if !values.UnknownType.Equals(strict.GetFlowedType()) {
		t.Fatalf("flowed type changed")
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

	orig := NewRecordQueryIndexPlan("idx_b", nil, []string{"T"}, values.UnknownType, true)
	strict := orig.WithStrictlySorted()

	if !strict.IsReverse() {
		t.Fatal("reverse flag should be preserved by WithStrictlySorted")
	}
	if !strict.IsStrictlySorted() {
		t.Fatal("should be strictlySorted")
	}
}
