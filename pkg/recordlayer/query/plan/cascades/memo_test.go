package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
)

func TestMemo_NewMemo_NilRoot(t *testing.T) {
	t.Parallel()
	m := NewMemo(nil)
	if m.Root() != nil {
		t.Fatal("expected nil root")
	}
	if len(m.References()) != 0 {
		t.Fatal("expected empty references")
	}
}

func TestMemo_NewMemo_IndexesDAG(t *testing.T) {
	t.Parallel()
	// Build: Filter(P, Scan)
	scan := expressions.NewFullUnorderedScanExpression([]string{"MyRecord"}, nil)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)},
		scanQ,
	)
	rootRef := expressions.InitialOf(filter)

	m := NewMemo(rootRef)
	if m.Root() != rootRef {
		t.Fatal("root mismatch")
	}
	if !m.ContainsReference(rootRef) {
		t.Fatal("root not indexed")
	}
	if !m.ContainsReference(scanRef) {
		t.Fatal("scanRef not indexed")
	}
	if len(m.References()) != 2 {
		t.Fatalf("expected 2 references, got %d", len(m.References()))
	}
}

func TestMemo_MemoizeExpression_LeafReuse(t *testing.T) {
	t.Parallel()
	// Two structurally-equal leaf scans should be memoized into the
	// same Reference.
	scan1 := expressions.NewFullUnorderedScanExpression([]string{"T"}, nil)
	scan2 := expressions.NewFullUnorderedScanExpression([]string{"T"}, nil)

	m := NewMemo(nil)
	ref1 := m.MemoizeExpression(scan1)
	ref2 := m.MemoizeExpression(scan2)

	if ref1 != ref2 {
		t.Fatal("expected same Reference for structurally-equal leaves")
	}
	if len(ref1.Members()) != 1 {
		t.Fatalf("expected 1 member, got %d", len(ref1.Members()))
	}
}

func TestMemo_MemoizeExpression_LeafDistinct(t *testing.T) {
	t.Parallel()
	// Two scans with different record types are different.
	scan1 := expressions.NewFullUnorderedScanExpression([]string{"A"}, nil)
	scan2 := expressions.NewFullUnorderedScanExpression([]string{"B"}, nil)

	m := NewMemo(nil)
	ref1 := m.MemoizeExpression(scan1)
	ref2 := m.MemoizeExpression(scan2)

	if ref1 == ref2 {
		t.Fatal("expected different References for different record types")
	}
}

func TestMemo_MemoizeExpression_NonLeafReuse(t *testing.T) {
	t.Parallel()
	// Two structurally-equal filters over the SAME child Reference
	// should be memoized into the same Reference.
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, nil)
	scanRef := expressions.InitialOf(scan)
	scanQ1 := expressions.ForEachQuantifier(scanRef)
	scanQ2 := expressions.ForEachQuantifier(scanRef)

	pred := []predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)}
	filter1 := expressions.NewLogicalFilterExpression(pred, scanQ1)
	filter2 := expressions.NewLogicalFilterExpression(pred, scanQ2)

	m := NewMemo(nil)
	// Register the scan Reference first (simulates prior memoization).
	m.RegisterReference(scanRef)

	ref1 := m.MemoizeExpression(filter1)
	ref2 := m.MemoizeExpression(filter2)

	if ref1 != ref2 {
		t.Fatal("expected same Reference for structurally-equal non-leaf expressions over same child")
	}
}

func TestMemo_MemoizeExpression_NonLeafDistinctChildren(t *testing.T) {
	t.Parallel()
	// Two filters that are structurally the same node-info but point to
	// DIFFERENT child References are in different equivalence classes.
	scanA := expressions.NewFullUnorderedScanExpression([]string{"A"}, nil)
	scanB := expressions.NewFullUnorderedScanExpression([]string{"B"}, nil)
	refA := expressions.InitialOf(scanA)
	refB := expressions.InitialOf(scanB)

	pred := []predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)}
	filter1 := expressions.NewLogicalFilterExpression(pred, expressions.ForEachQuantifier(refA))
	filter2 := expressions.NewLogicalFilterExpression(pred, expressions.ForEachQuantifier(refB))

	m := NewMemo(nil)
	m.RegisterReference(refA)
	m.RegisterReference(refB)

	ref1 := m.MemoizeExpression(filter1)
	ref2 := m.MemoizeExpression(filter2)

	if ref1 == ref2 {
		t.Fatal("expected different References for expressions with different children")
	}
}

func TestMemo_MemoizeExpression_IntegrationWithPlanner(t *testing.T) {
	t.Parallel()
	// Build a simple tree and verify the Planner's Memo is populated.
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, nil)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)},
		scanQ,
	)
	rootRef := expressions.InitialOf(filter)

	p := NewPlanner(DefaultExpressionRules(), nil)
	_, converged := exploreRewriting(p, rootRef)
	if !converged {
		t.Fatal("expected convergence")
	}
	if p.Memo() == nil {
		t.Fatal("expected non-nil Memo after exploration")
	}
	if !p.Memo().ContainsReference(rootRef) {
		t.Fatal("root not in Memo")
	}
	if !p.Memo().ContainsReference(scanRef) {
		t.Fatal("scanRef not in Memo")
	}
}

func TestMemo_MemoizeExpressions_Batch(t *testing.T) {
	t.Parallel()
	scan1 := expressions.NewFullUnorderedScanExpression([]string{"T"}, nil)
	scan2 := expressions.NewFullUnorderedScanExpression([]string{"T"}, nil)

	m := NewMemo(nil)
	ref := m.MemoizeExpressions([]expressions.RelationalExpression{scan1, scan2})

	// Both go into the same Reference (they're structurally equal leaves).
	if len(ref.Members()) != 1 {
		t.Fatalf("expected 1 member (dedup'd), got %d", len(ref.Members()))
	}

	// Memoizing again returns the same Reference.
	scan3 := expressions.NewFullUnorderedScanExpression([]string{"T"}, nil)
	ref2 := m.MemoizeExpression(scan3)
	if ref != ref2 {
		t.Fatal("expected same Reference from subsequent memoize")
	}
}

func TestMemo_AddExpression_UpdatesIndex(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, nil)
	scanRef := expressions.InitialOf(scan)

	m := NewMemo(nil)
	m.RegisterReference(scanRef)

	// Now create a filter over scanRef and add it to a new Reference.
	pred := []predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)}
	q := expressions.ForEachQuantifier(scanRef)
	filter := expressions.NewLogicalFilterExpression(pred, q)
	filterRef := expressions.InitialOf(filter)
	m.RegisterReference(filterRef)

	// The childToParents index should show scanRef → filterRef.
	edges := m.childToParents[scanRef]
	found := false
	for _, e := range edges {
		if e.parent == filterRef {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected scanRef to have filterRef as parent in index")
	}

	// Now memoize a second filter with same pred over the same scanRef —
	// should find filterRef.
	q2 := expressions.ForEachQuantifier(scanRef)
	filter2 := expressions.NewLogicalFilterExpression(pred, q2)
	ref := m.MemoizeExpression(filter2)
	if ref != filterRef {
		t.Fatal("expected memoization to reuse filterRef")
	}
}

func TestMemo_CrossReferenceSharing_ThroughRules(t *testing.T) {
	t.Parallel()
	// Scenario: two different paths in the DAG independently produce
	// the same sub-expression. The Memo should route them to the same
	// Reference.
	//
	// Build: Union(Filter(P, Scan("T")), Sort(Filter(P, Scan("T"))))
	// Both branches have "Filter(P, Scan("T"))" — with memoization,
	// they should share the same Reference for the Filter node.

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, nil)

	m := NewMemo(nil)
	scanRef := m.MemoizeExpression(scan)

	pred := []predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)}

	// First branch creates Filter(P, scanRef).
	q1 := expressions.ForEachQuantifier(scanRef)
	filter1 := expressions.NewLogicalFilterExpression(pred, q1)
	filterRef1 := m.MemoizeExpression(filter1)

	// Second branch independently creates Filter(P, scanRef).
	q2 := expressions.ForEachQuantifier(scanRef)
	filter2 := expressions.NewLogicalFilterExpression(pred, q2)
	filterRef2 := m.MemoizeExpression(filter2)

	// They should be the same Reference.
	if filterRef1 != filterRef2 {
		t.Fatal("expected cross-Reference sharing for identical Filter sub-expressions")
	}
}

func TestMemo_ExpressionRuleCall_MemoizeExpression_WithMemo(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, nil)
	scanRef := expressions.InitialOf(scan)

	// Create a parent Reference (the one the rule fires on) that is
	// DIFFERENT from scanRef — otherwise the self-reference guard triggers.
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)},
		expressions.ForEachQuantifier(scanRef),
	)
	parentRef := expressions.InitialOf(filter)

	m := NewMemo(nil)
	m.RegisterReference(parentRef)

	// Simulate a rule call on the parent Reference.
	call := NewExpressionRuleCallWithMemo(parentRef, nil, nil, m)

	// MemoizeExpression should find scanRef via the Memo.
	scan2 := expressions.NewFullUnorderedScanExpression([]string{"T"}, nil)
	ref := call.MemoizeExpression(scan2)
	if ref != scanRef {
		t.Fatal("expected MemoizeExpression via rule call to find existing scanRef")
	}
}

func TestMemo_ExpressionRuleCall_SelfRefGuard(t *testing.T) {
	t.Parallel()
	// When the Memo would return the SAME Reference the rule is
	// yielding into, the self-reference guard creates a fresh Reference
	// to prevent cycles.
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, nil)
	scanRef := expressions.InitialOf(scan)

	m := NewMemo(nil)
	m.RegisterReference(scanRef)

	call := NewExpressionRuleCallWithMemo(scanRef, nil, nil, m)
	scan2 := expressions.NewFullUnorderedScanExpression([]string{"T"}, nil)
	ref := call.MemoizeExpression(scan2)
	// Guard prevents returning scanRef (would be a cycle).
	if ref == scanRef {
		t.Fatal("self-reference guard should prevent returning call.Reference")
	}
	if len(ref.Members()) != 1 {
		t.Fatalf("expected fresh single-member ref, got %d members", len(ref.Members()))
	}
}

func TestMemo_ExpressionRuleCall_MemoizeExpression_WithoutMemo(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, nil)
	scanRef := expressions.InitialOf(scan)

	// No Memo — should fall back to InitialOf.
	call := NewExpressionRuleCall(scanRef, nil, nil)
	scan2 := expressions.NewFullUnorderedScanExpression([]string{"T"}, nil)
	ref := call.MemoizeExpression(scan2)
	// Without Memo, a fresh Reference is created.
	if ref == scanRef {
		t.Fatal("without Memo, MemoizeExpression should create a fresh Reference")
	}
	if len(ref.Members()) != 1 {
		t.Fatalf("expected 1 member in fresh ref, got %d", len(ref.Members()))
	}
}

func TestMemo_PanicOnNilExpression(t *testing.T) {
	t.Parallel()
	m := NewMemo(nil)

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on nil expression")
		}
	}()
	m.MemoizeExpression(nil)
}
