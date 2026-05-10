package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

func TestExpandValueIndex_TwoColumns(t *testing.T) {
	t.Parallel()

	alias0 := values.UniqueCorrelationIdentifier()
	alias1 := values.UniqueCorrelationIdentifier()
	cand := NewValueIndexScanMatchCandidate(
		"idx_order_region_amount",
		[]string{"Order"},
		[]string{"region", "amount"},
		[]values.CorrelationIdentifier{alias0, alias1},
		values.UnknownType,
		false,
	)

	trav := ExpandValueIndex(cand)
	if trav == nil {
		t.Fatal("ExpandValueIndex returned nil")
	}

	rootRef := trav.GetRootReference()
	if rootRef == nil {
		t.Fatal("root reference is nil")
	}

	// Root expression should be MatchableSortExpression.
	members := rootRef.AllMembers()
	if len(members) != 1 {
		t.Fatalf("root ref members: got %d, want 1", len(members))
	}
	matchSort, ok := members[0].(*expressions.MatchableSortExpression)
	if !ok {
		t.Fatalf("root expression: got %T, want *MatchableSortExpression", members[0])
	}

	// Sort parameter IDs should match sargable aliases.
	sortIDs := matchSort.GetSortParameterIDs()
	if len(sortIDs) != 2 {
		t.Fatalf("sort param IDs: got %d, want 2", len(sortIDs))
	}
	if sortIDs[0] != alias0 || sortIDs[1] != alias1 {
		t.Fatalf("sort param IDs mismatch: got %v, want [%s, %s]", sortIDs, alias0, alias1)
	}
	if matchSort.IsReverse() {
		t.Fatal("isReverse: got true, want false")
	}

	// Inner quantifier leads to SelectExpression.
	innerQ := matchSort.GetInner()
	innerRef := innerQ.GetRangesOver()
	innerMembers := innerRef.AllMembers()
	if len(innerMembers) != 1 {
		t.Fatalf("inner ref members: got %d, want 1", len(innerMembers))
	}
	selectExpr, ok := innerMembers[0].(*expressions.SelectExpression)
	if !ok {
		t.Fatalf("inner expression: got %T, want *SelectExpression", innerMembers[0])
	}

	// SelectExpression should have 2 predicates (Placeholders).
	preds := selectExpr.GetPredicates()
	if len(preds) != 2 {
		t.Fatalf("predicates: got %d, want 2", len(preds))
	}
	for i, pred := range preds {
		ph, ok := pred.(*predicates.Placeholder)
		if !ok {
			t.Fatalf("predicate[%d]: got %T, want *Placeholder", i, pred)
		}
		fv, ok := ph.Value.(*values.FieldValue)
		if !ok {
			t.Fatalf("placeholder[%d] value: got %T, want *FieldValue", i, ph.Value)
		}
		expectedCol := []string{"region", "amount"}[i]
		if fv.Field != expectedCol {
			t.Fatalf("placeholder[%d] field: got %q, want %q", i, fv.Field, expectedCol)
		}
		expectedAlias := []values.CorrelationIdentifier{alias0, alias1}[i]
		if ph.ParameterAlias != expectedAlias {
			t.Fatalf("placeholder[%d] alias: got %s, want %s", i, ph.ParameterAlias, expectedAlias)
		}
	}

	// SelectExpression should have 1 quantifier (the ForEach over the scan).
	selectQuants := selectExpr.GetQuantifiers()
	if len(selectQuants) != 1 {
		t.Fatalf("select quantifiers: got %d, want 1", len(selectQuants))
	}

	// The base scan should be a FullUnorderedScanExpression.
	scanRef := selectQuants[0].GetRangesOver()
	scanMembers := scanRef.AllMembers()
	if len(scanMembers) != 1 {
		t.Fatalf("scan ref members: got %d, want 1", len(scanMembers))
	}
	scan, ok := scanMembers[0].(*expressions.FullUnorderedScanExpression)
	if !ok {
		t.Fatalf("scan expression: got %T, want *FullUnorderedScanExpression", scanMembers[0])
	}
	if rt := scan.GetRecordTypes(); len(rt) != 1 || rt[0] != "Order" {
		t.Fatalf("scan record types: got %v, want [Order]", rt)
	}
}

func TestExpandValueIndex_ZeroColumns(t *testing.T) {
	t.Parallel()

	cand := NewValueIndexScanMatchCandidate(
		"idx_empty",
		[]string{"Customer"},
		[]string{},
		[]values.CorrelationIdentifier{},
		values.UnknownType,
		false,
	)

	trav := ExpandValueIndex(cand)
	if trav == nil {
		t.Fatal("ExpandValueIndex returned nil")
	}

	rootRef := trav.GetRootReference()
	members := rootRef.AllMembers()
	if len(members) != 1 {
		t.Fatalf("root ref members: got %d, want 1", len(members))
	}
	matchSort, ok := members[0].(*expressions.MatchableSortExpression)
	if !ok {
		t.Fatalf("root expression: got %T, want *MatchableSortExpression", members[0])
	}
	if len(matchSort.GetSortParameterIDs()) != 0 {
		t.Fatalf("sort param IDs: got %d, want 0", len(matchSort.GetSortParameterIDs()))
	}

	// Inner should be a SelectExpression with no predicates.
	innerQ := matchSort.GetInner()
	innerMembers := innerQ.GetRangesOver().AllMembers()
	selectExpr, ok := innerMembers[0].(*expressions.SelectExpression)
	if !ok {
		t.Fatalf("inner expression: got %T, want *SelectExpression", innerMembers[0])
	}
	if len(selectExpr.GetPredicates()) != 0 {
		t.Fatalf("predicates: got %d, want 0", len(selectExpr.GetPredicates()))
	}
}

func TestValueIndexScanMatchCandidate_GetTraversal_NonNil(t *testing.T) {
	t.Parallel()

	cand := NewValueIndexScanMatchCandidate(
		"idx_name",
		[]string{"Person"},
		[]string{"name"},
		[]values.CorrelationIdentifier{values.UniqueCorrelationIdentifier()},
		values.UnknownType,
		false,
	)

	trav := cand.GetTraversal()
	if trav == nil {
		t.Fatal("GetTraversal returned nil")
	}
	if trav.GetRootReference() == nil {
		t.Fatal("traversal root reference is nil")
	}
}

func TestValueIndexScanMatchCandidate_GetTraversal_SyncOnce(t *testing.T) {
	t.Parallel()

	cand := NewValueIndexScanMatchCandidate(
		"idx_city",
		[]string{"Address"},
		[]string{"city", "zip"},
		[]values.CorrelationIdentifier{
			values.UniqueCorrelationIdentifier(),
			values.UniqueCorrelationIdentifier(),
		},
		values.UnknownType,
		true,
	)

	trav1 := cand.GetTraversal()
	trav2 := cand.GetTraversal()
	if trav1 != trav2 {
		t.Fatal("GetTraversal returned different pointers on repeated calls (sync.Once violated)")
	}
}

func TestExpandValueIndex_LeafReferences(t *testing.T) {
	t.Parallel()

	cand := NewValueIndexScanMatchCandidate(
		"idx_leaf",
		[]string{"Item"},
		[]string{"price"},
		[]values.CorrelationIdentifier{values.UniqueCorrelationIdentifier()},
		values.UnknownType,
		false,
	)

	trav := ExpandValueIndex(cand)
	leafRefs := trav.GetLeafReferences()
	if len(leafRefs) == 0 {
		t.Fatal("expected at least one leaf reference")
	}

	// The leaf should contain the FullUnorderedScanExpression.
	foundScan := false
	for _, ref := range leafRefs {
		for _, expr := range ref.AllMembers() {
			if _, ok := expr.(*expressions.FullUnorderedScanExpression); ok {
				foundScan = true
			}
		}
	}
	if !foundScan {
		t.Fatal("no FullUnorderedScanExpression found in leaf references")
	}
}

func TestAggregateIndexMatchCandidate_GetTraversal_NonNil(t *testing.T) {
	t.Parallel()

	cand := NewAggregateIndexMatchCandidate(
		"idx_sum_region",
		[]string{"Order"},
		[]string{"region"},
		expressions.AggSum,
		"amount",
	)

	trav := cand.GetTraversal()
	if trav == nil {
		t.Fatal("AggregateIndexMatchCandidate.GetTraversal returned nil")
	}
	if trav.GetRootReference() == nil {
		t.Fatal("traversal root reference is nil")
	}

	// Should have a MatchableSortExpression at root.
	members := trav.GetRootReference().AllMembers()
	if len(members) != 1 {
		t.Fatalf("root ref members: got %d, want 1", len(members))
	}
	if _, ok := members[0].(*expressions.MatchableSortExpression); !ok {
		t.Fatalf("root expression: got %T, want *MatchableSortExpression", members[0])
	}
}

func TestAggregateIndexMatchCandidate_GetTraversal_SyncOnce(t *testing.T) {
	t.Parallel()

	cand := NewAggregateIndexMatchCandidate(
		"idx_count",
		[]string{"Event"},
		[]string{"category"},
		expressions.AggCount,
		"id",
	)

	trav1 := cand.GetTraversal()
	trav2 := cand.GetTraversal()
	if trav1 != trav2 {
		t.Fatal("AggregateIndexMatchCandidate.GetTraversal returned different pointers on repeated calls")
	}
}
