package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// helper: build a single scan leaf wrapped in a Reference.
func testScanRef() (*expressions.FullUnorderedScanExpression, *expressions.Reference) {
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, nil)
	ref := expressions.InitialOf(scan)
	return scan, ref
}

// helper: build a filter over a child quantifier.
func testFilterOver(childRef *expressions.Reference) (*expressions.LogicalFilterExpression, expressions.Quantifier, *expressions.Reference) {
	q := expressions.ForEachQuantifier(childRef)
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)},
		q,
	)
	ref := expressions.InitialOf(filter)
	return filter, q, ref
}

// helper: build a sort over a child quantifier.
func testSortOver(childRef *expressions.Reference) (*expressions.LogicalSortExpression, expressions.Quantifier, *expressions.Reference) {
	q := expressions.ForEachQuantifier(childRef)
	sort := expressions.NewLogicalSortExpression(nil, q)
	ref := expressions.InitialOf(sort)
	return sort, q, ref
}

func TestTraversal_SingleLeaf(t *testing.T) {
	t.Parallel()

	_, scanRef := testScanRef()
	tr := NewTraversal(scanRef)

	if tr.GetRootReference() != scanRef {
		t.Fatal("root reference mismatch")
	}

	leafRefs := tr.GetLeafReferences()
	if len(leafRefs) != 1 {
		t.Fatalf("expected 1 leaf ref, got %d", len(leafRefs))
	}
	if leafRefs[0] != scanRef {
		t.Fatal("leaf ref should be the scan ref")
	}

	// No parents for the root (it's the only node).
	parents := tr.GetParentRefPairs(scanRef)
	if len(parents) != 0 {
		t.Fatalf("expected 0 parents for root leaf, got %d", len(parents))
	}
}

func TestTraversal_FilterOverScan(t *testing.T) {
	t.Parallel()

	scan, scanRef := testScanRef()
	filter, _, filterRef := testFilterOver(scanRef)

	tr := NewTraversal(filterRef)

	if tr.GetRootReference() != filterRef {
		t.Fatal("root reference mismatch")
	}

	// Leaf refs: only scanRef (scan has no quantifiers).
	leafRefs := tr.GetLeafReferences()
	if len(leafRefs) != 1 {
		t.Fatalf("expected 1 leaf ref, got %d", len(leafRefs))
	}
	if leafRefs[0] != scanRef {
		t.Fatal("leaf ref should be the scan ref")
	}

	// scanRef's parent should be (filterRef, filter).
	parents := tr.GetParentRefPairs(scanRef)
	if len(parents) != 1 {
		t.Fatalf("expected 1 parent of scanRef, got %d", len(parents))
	}
	if parents[0].ref != filterRef {
		t.Fatal("parent ref should be filterRef")
	}
	if parents[0].expr != filter {
		t.Fatal("parent expr should be the filter expression")
	}

	// filterRef has no parents (it's the root).
	if len(tr.GetParentRefPairs(filterRef)) != 0 {
		t.Fatal("root ref should have no parents")
	}

	_ = scan // used to verify identity
}

func TestTraversal_FindReferencingExpressions(t *testing.T) {
	t.Parallel()

	_, scanRef := testScanRef()
	filter, _, filterRef := testFilterOver(scanRef)

	tr := NewTraversal(filterRef)

	// Given the scan's ref, find the filter as the referencing expression.
	result := tr.FindReferencingExpressions([]*expressions.Reference{scanRef})
	if len(result) != 1 {
		t.Fatalf("expected 1 parent ref in result, got %d", len(result))
	}
	exprs, ok := result[filterRef]
	if !ok {
		t.Fatal("expected filterRef in result map")
	}
	if len(exprs) != 1 {
		t.Fatalf("expected 1 expression for filterRef, got %d", len(exprs))
	}
	if exprs[0] != filter {
		t.Fatal("expression should be the filter")
	}
}

func TestTraversal_ThreeLevelTree(t *testing.T) {
	t.Parallel()

	// scan -> filter -> sort
	scan, scanRef := testScanRef()
	filter, _, filterRef := testFilterOver(scanRef)
	sort, _, sortRef := testSortOver(filterRef)

	tr := NewTraversal(sortRef)

	// Root is sortRef.
	if tr.GetRootReference() != sortRef {
		t.Fatal("root reference mismatch")
	}

	// Leaf: only scanRef.
	leafRefs := tr.GetLeafReferences()
	if len(leafRefs) != 1 {
		t.Fatalf("expected 1 leaf ref, got %d", len(leafRefs))
	}
	if leafRefs[0] != scanRef {
		t.Fatal("leaf ref should be scanRef")
	}

	// scan's parent = filter in filterRef.
	scanParents := tr.GetParentRefPairs(scanRef)
	if len(scanParents) != 1 {
		t.Fatalf("expected 1 parent for scanRef, got %d", len(scanParents))
	}
	if scanParents[0].ref != filterRef || scanParents[0].expr != filter {
		t.Fatal("scan's parent should be (filterRef, filter)")
	}

	// filter's parent = sort in sortRef.
	filterParents := tr.GetParentRefPairs(filterRef)
	if len(filterParents) != 1 {
		t.Fatalf("expected 1 parent for filterRef, got %d", len(filterParents))
	}
	if filterParents[0].ref != sortRef || filterParents[0].expr != sort {
		t.Fatal("filter's parent should be (sortRef, sort)")
	}

	// FindReferencingExpressions from scanRef should yield filterRef->filter.
	result := tr.FindReferencingExpressions([]*expressions.Reference{scanRef})
	if len(result) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(result))
	}
	if result[filterRef][0] != filter {
		t.Fatal("expected filter as referencing expression")
	}

	// FindReferencingExpressions from filterRef should yield sortRef->sort.
	result2 := tr.FindReferencingExpressions([]*expressions.Reference{filterRef})
	if len(result2) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(result2))
	}
	if result2[sortRef][0] != sort {
		t.Fatal("expected sort as referencing expression")
	}

	// FindReferencingExpressions from both should yield both parents.
	result3 := tr.FindReferencingExpressions([]*expressions.Reference{scanRef, filterRef})
	if len(result3) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(result3))
	}

	_ = scan // identity verification
}

func TestTraversal_EmptyReference(t *testing.T) {
	t.Parallel()

	// An empty reference has no members — the traversal should work
	// but produce empty results.
	ref := &expressions.Reference{}
	tr := NewTraversal(ref)

	if tr.GetRootReference() != ref {
		t.Fatal("root reference mismatch")
	}

	// No leaf refs — no members means no expressions at all.
	if len(tr.GetLeafReferences()) != 0 {
		t.Fatalf("expected 0 leaf refs for empty ref, got %d", len(tr.GetLeafReferences()))
	}

	// FindReferencingExpressions on empty input returns empty.
	result := tr.FindReferencingExpressions(nil)
	if len(result) != 0 {
		t.Fatal("expected empty result for nil childRefs")
	}
}

func TestTraversal_DAG_SharedChild(t *testing.T) {
	t.Parallel()

	// Build a DAG: two filter expressions in the SAME parent reference,
	// both ranging over the same scanRef via different quantifiers.
	// This tests that the shared child reference is visited only once
	// and both parents are recorded.
	scan, scanRef := testScanRef()

	q1 := expressions.ForEachQuantifier(scanRef)
	filter1 := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)},
		q1,
	)
	q2 := expressions.ForEachQuantifier(scanRef)
	filter2 := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriFalse)},
		q2,
	)

	// Put both filters in the same reference.
	parentRef := expressions.InitialOf(filter1)
	parentRef.Insert(filter2)

	tr := NewTraversal(parentRef)

	// Root is parentRef.
	if tr.GetRootReference() != parentRef {
		t.Fatal("root reference mismatch")
	}

	// Leaf: scanRef.
	leafRefs := tr.GetLeafReferences()
	if len(leafRefs) != 1 {
		t.Fatalf("expected 1 leaf ref, got %d", len(leafRefs))
	}
	if leafRefs[0] != scanRef {
		t.Fatal("leaf ref should be scanRef")
	}

	// scanRef should have 2 parents: (parentRef, filter1) and (parentRef, filter2).
	parents := tr.GetParentRefPairs(scanRef)
	if len(parents) != 2 {
		t.Fatalf("expected 2 parents for scanRef, got %d", len(parents))
	}

	foundFilter1, foundFilter2 := false, false
	for _, p := range parents {
		if p.ref != parentRef {
			t.Fatal("both parents should be in parentRef")
		}
		if p.expr == filter1 {
			foundFilter1 = true
		}
		if p.expr == filter2 {
			foundFilter2 = true
		}
	}
	if !foundFilter1 || !foundFilter2 {
		t.Fatal("both filter1 and filter2 should appear as parents")
	}

	// FindReferencingExpressions deduplicates: asking for scanRef should
	// yield parentRef -> [filter1, filter2] (both, no duplicates).
	result := tr.FindReferencingExpressions([]*expressions.Reference{scanRef})
	if len(result) != 1 {
		t.Fatalf("expected 1 ref key, got %d", len(result))
	}
	exprs := result[parentRef]
	if len(exprs) != 2 {
		t.Fatalf("expected 2 expressions, got %d", len(exprs))
	}

	_ = scan // identity verification
}

func TestTraversal_FindReferencingExpressions_Dedup(t *testing.T) {
	t.Parallel()

	// Build: scan1Ref and scan2Ref are both children of a select
	// expression (which has two quantifiers). Calling
	// FindReferencingExpressions with both child refs should yield the
	// parent expression only once (dedup).
	scan1 := expressions.NewFullUnorderedScanExpression([]string{"T1"}, nil)
	scan1Ref := expressions.InitialOf(scan1)
	scan2 := expressions.NewFullUnorderedScanExpression([]string{"T2"}, nil)
	scan2Ref := expressions.InitialOf(scan2)

	q1 := expressions.ForEachQuantifier(scan1Ref)
	q2 := expressions.ForEachQuantifier(scan2Ref)

	// Use a SelectExpression which supports multiple quantifiers.
	sel := expressions.NewSelectExpression(
		q1.GetFlowedObjectValue(),
		[]expressions.Quantifier{q1, q2},
		nil,
	)
	selRef := expressions.InitialOf(sel)

	tr := NewTraversal(selRef)

	// Both scan refs are leaves.
	leafRefs := tr.GetLeafReferences()
	if len(leafRefs) != 2 {
		t.Fatalf("expected 2 leaf refs, got %d", len(leafRefs))
	}

	// FindReferencingExpressions with both children yields the select
	// once, not twice.
	result := tr.FindReferencingExpressions([]*expressions.Reference{scan1Ref, scan2Ref})
	if len(result) != 1 {
		t.Fatalf("expected 1 parent ref, got %d", len(result))
	}
	exprs := result[selRef]
	if len(exprs) != 1 {
		t.Fatalf("expected 1 expression (deduplicated), got %d", len(exprs))
	}
	if exprs[0] != sel {
		t.Fatal("expression should be the select")
	}
}

func TestTraversal_MatchCandidate_GetTraversal_NonNil(t *testing.T) {
	t.Parallel()

	// Both match candidate types now build traversals lazily via
	// ExpandValueIndex. Verify they return non-nil with correct root.
	alias := values.UniqueCorrelationIdentifier()
	vc := NewValueIndexScanMatchCandidate("idx", []string{"T"}, []string{"a"},
		[]values.CorrelationIdentifier{alias}, nil, false, nil)
	trav := vc.GetTraversal()
	if trav == nil {
		t.Fatal("expected non-nil traversal from ValueIndexScanMatchCandidate")
	}
	if trav.GetRootReference() == nil {
		t.Fatal("expected non-nil root reference")
	}

	ac := NewAggregateIndexMatchCandidate("agg_idx", []string{"T"}, []string{"g"}, expressions.AggSum, "v")
	aggTrav := ac.GetTraversal()
	if aggTrav == nil {
		t.Fatal("expected non-nil traversal from AggregateIndexMatchCandidate")
	}
	if aggTrav.GetRootReference() == nil {
		t.Fatal("expected non-nil root reference")
	}
}
