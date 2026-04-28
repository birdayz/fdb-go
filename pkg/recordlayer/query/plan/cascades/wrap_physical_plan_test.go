package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// TestWrapPhysicalPlan_Scan pins the leaf case.
func TestWrapPhysicalPlan_Scan(t *testing.T) {
	t.Parallel()
	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	wrap := wrapPhysicalPlan(scan)
	if wrap == nil {
		t.Fatal("wrapPhysicalPlan(Scan) = nil")
	}
	if _, ok := wrap.(*physicalScanWrapper); !ok {
		t.Fatalf("wrap = %T, want *physicalScanWrapper", wrap)
	}
}

// TestWrapPhysicalPlan_Filter pins the recursive-wrap path.
func TestWrapPhysicalPlan_Filter(t *testing.T) {
	t.Parallel()
	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	filter := plans.NewRecordQueryFilterPlan([]predicates.QueryPredicate{pred}, scan)
	wrap := wrapPhysicalPlan(filter)
	if wrap == nil {
		t.Fatal("wrapPhysicalPlan(Filter) = nil")
	}
	if _, ok := wrap.(*physicalFilterWrapper); !ok {
		t.Fatalf("wrap = %T, want *physicalFilterWrapper", wrap)
	}
}

// TestWrapPhysicalPlan_Union pins the N-children wrap path with
// concatenated quantifiers.
func TestWrapPhysicalPlan_Union(t *testing.T) {
	t.Parallel()
	scanA := plans.NewRecordQueryScanPlan([]string{"A"}, values.UnknownType, false)
	scanB := plans.NewRecordQueryScanPlan([]string{"B"}, values.UnknownType, false)
	union := plans.NewRecordQueryUnionPlan([]plans.RecordQueryPlan{scanA, scanB})
	wrap := wrapPhysicalPlan(union)
	if wrap == nil {
		t.Fatal("wrapPhysicalPlan(Union) = nil")
	}
	uw, ok := wrap.(*physicalUnionWrapper)
	if !ok {
		t.Fatalf("wrap = %T, want *physicalUnionWrapper", wrap)
	}
	if got := len(uw.GetQuantifiers()); got != 2 {
		t.Fatalf("union wrapper has %d quantifiers, want 2", got)
	}
}

// TestWrapPhysicalPlan_Intersection pins the N-children wrap path
// for Intersection — symmetric with Union but verifies the
// review-feedback fix for the missing IntersectionPlan case in
// wrapPhysicalPlan.
func TestWrapPhysicalPlan_Intersection(t *testing.T) {
	t.Parallel()
	scanA := plans.NewRecordQueryScanPlan([]string{"A"}, values.UnknownType, false)
	scanB := plans.NewRecordQueryScanPlan([]string{"B"}, values.UnknownType, false)
	keys := []values.Value{
		&values.FieldValue{Field: "id", Typ: values.NotNullLong},
	}
	intersection := plans.NewRecordQueryIntersectionPlan(
		[]plans.RecordQueryPlan{scanA, scanB}, keys)
	wrap := wrapPhysicalPlan(intersection)
	if wrap == nil {
		t.Fatal("wrapPhysicalPlan(Intersection) = nil")
	}
	iw, ok := wrap.(*physicalIntersectionWrapper)
	if !ok {
		t.Fatalf("wrap = %T, want *physicalIntersectionWrapper", wrap)
	}
	if got := len(iw.GetQuantifiers()); got != 2 {
		t.Fatalf("intersection wrapper has %d quantifiers, want 2", got)
	}
}

// TestWrapPhysicalPlan_NestedUnionInIntersection pins the recursive
// wrap path: Intersection(Union(Scan, Scan), Scan) — once the
// review-feedback fix lifts the wrapper-symmetry block, this
// shape can fully wrap.
func TestWrapPhysicalPlan_NestedUnionInIntersection(t *testing.T) {
	t.Parallel()
	scanA := plans.NewRecordQueryScanPlan([]string{"A"}, values.UnknownType, false)
	scanB := plans.NewRecordQueryScanPlan([]string{"B"}, values.UnknownType, false)
	scanC := plans.NewRecordQueryScanPlan([]string{"C"}, values.UnknownType, false)
	innerUnion := plans.NewRecordQueryUnionPlan([]plans.RecordQueryPlan{scanA, scanB})
	outer := plans.NewRecordQueryIntersectionPlan(
		[]plans.RecordQueryPlan{innerUnion, scanC},
		nil)
	wrap := wrapPhysicalPlan(outer)
	if wrap == nil {
		t.Fatal("wrapPhysicalPlan(Intersection(Union, Scan)) = nil — recursive wrap broke")
	}
	if _, ok := wrap.(*physicalIntersectionWrapper); !ok {
		t.Fatalf("outer wrap = %T, want *physicalIntersectionWrapper", wrap)
	}
}

// TestWrapPhysicalPlan_NilForUnknownPlan pins the fallback path —
// returns nil if the concrete plan type isn't recognised.
func TestWrapPhysicalPlan_NilForUnknownPlan(t *testing.T) {
	t.Parallel()
	// Pass a deliberately-nil plan to exercise the type-switch
	// fall-through. The function should return nil rather than panic.
	var p plans.RecordQueryPlan
	if got := wrapPhysicalPlan(p); got != nil {
		t.Fatalf("wrapPhysicalPlan(nil) = %v, want nil", got)
	}
}

// Use ForEachQuantifier to acknowledge unused-import suppression
// on legacy build configs.
var _ = expressions.ForEachQuantifier
