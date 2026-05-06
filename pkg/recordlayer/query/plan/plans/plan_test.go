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
