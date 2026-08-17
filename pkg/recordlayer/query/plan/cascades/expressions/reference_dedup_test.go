package expressions

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
)

// TestReference_Insert_DistinctWhenChildRefsDiffer pins the
// children-aware dedup contract: two filters with the same
// EqualsWithoutChildren but different child Reference pointers are
// NOT duplicates.
func TestReference_Insert_DistinctWhenChildRefsDiffer(t *testing.T) {
	t.Parallel()
	scanA := mustExpression(NewFullUnorderedScanExpression([]string{"A"}, testRecordType()))
	scanB := mustExpression(NewFullUnorderedScanExpression([]string{"B"}, testRecordType()))
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	// Two filters with the SAME predicate but DIFFERENT inner Refs.
	fA := mustExpression(NewLogicalFilterExpression([]predicates.QueryPredicate{pT}, ForEachQuantifier(InitialOf(scanA))))
	fB := mustExpression(NewLogicalFilterExpression([]predicates.QueryPredicate{pT}, ForEachQuantifier(InitialOf(scanB))))
	r := InitialOf(fA)
	if !r.Insert(fB) {
		t.Fatal("insert dedup'd a filter over a different inner Reference")
	}
	if got := len(r.Members()); got != 2 {
		t.Fatalf("members=%d, want 2", got)
	}
}

// TestReference_Insert_DedupSameChildRefs pins that same-predicate +
// same-child-Reference IS a duplicate (the dedup still fires on
// genuinely-equivalent expressions).
func TestReference_Insert_DedupSameChildRefs(t *testing.T) {
	t.Parallel()
	scan := mustExpression(NewFullUnorderedScanExpression([]string{"A"}, testRecordType()))
	scanRef := InitialOf(scan)
	// Two filters with the same predicate AND the same inner Reference
	// pointer (same scanRef).
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	fA := mustExpression(NewLogicalFilterExpression([]predicates.QueryPredicate{pT}, ForEachQuantifier(scanRef)))
	fB := mustExpression(NewLogicalFilterExpression([]predicates.QueryPredicate{pT}, ForEachQuantifier(scanRef)))
	r := InitialOf(fA)
	if r.Insert(fB) {
		t.Fatal("insert added a structurally+children-equivalent duplicate")
	}
	if got := len(r.Members()); got != 1 {
		t.Fatalf("members=%d, want 1 (dedup absorbed)", got)
	}
}
