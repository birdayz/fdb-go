package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// truePred returns a fresh always-true constant predicate.
func truePred() predicates.QueryPredicate { return predicates.NewConstantPredicate(predicates.TriTrue) }

// filterOver builds Filter(TRUE, →inner) as a single-member Reference.
func filterOver(inner *expressions.Reference) *expressions.Reference {
	f := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{truePred()},
		expressions.ForEachQuantifier(inner),
	)
	return expressions.InitialOf(f)
}

// freshFilterMember builds a standalone Filter(TRUE, →inner) expression
// (not wrapped in a Reference) — a stand-in for a rule's yielded output.
func freshFilterMember(inner *expressions.Reference) expressions.RelationalExpression {
	return expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{truePred()},
		expressions.ForEachQuantifier(inner),
	)
}

// TestMemoMerge_SiblingEquivalentGroupsMerge proves the core optimization:
// two independently-created sibling References holding structurally-equal
// members (over the same child) collapse into one when a yield surfaces
// the equivalence. This is the redundant-subexpression elimination the
// feature exists for.
func TestMemoMerge_SiblingEquivalentGroupsMerge(t *testing.T) {
	t.Parallel()
	scanRef := expressions.InitialOf(fixtureScan("T"))
	l := filterOver(scanRef) // Filter(TRUE, scanT)
	r := filterOver(scanRef) // structurally identical, distinct Reference

	m := NewMemo(nil)
	m.RegisterReference(l) // lower id ⇒ survivor
	m.RegisterReference(r)

	if m.MergeCount() != 0 {
		t.Fatalf("no merge expected before integration, got %d", m.MergeCount())
	}

	// A rule yields, into r, an expression equal to l's member.
	m.Integrate(r, freshFilterMember(scanRef))

	if m.MergeCount() != 1 {
		t.Fatalf("expected exactly 1 merge, got %d", m.MergeCount())
	}
	if l.Canonical() != l {
		t.Fatal("the lower-id Reference (l) must be the survivor")
	}
	if r.Canonical() != l {
		t.Fatal("r must forward to the survivor l after merge")
	}
}

// TestMemoMerge_InFlightTaskNotStranded is the Torvalds-#1 regression: a
// task still pointing at the merged-away (loser) Reference must keep
// working via transparent forwarding. A distinct member carried by the
// loser is preserved (by pointer) in the survivor, so ContainsExactly —
// the gate every TransformExprTask checks — still returns true.
func TestMemoMerge_InFlightTaskNotStranded(t *testing.T) {
	t.Parallel()
	scanRef := expressions.InitialOf(fixtureScan("T"))
	l := filterOver(scanRef)
	r := filterOver(scanRef)
	// Give r a distinct extra member that l does not have.
	extra := expressions.NewLogicalSortExpression(nil, expressions.ForEachQuantifier(scanRef))
	r.Insert(extra)

	m := NewMemo(nil)
	m.RegisterReference(l)
	m.RegisterReference(r)
	m.Integrate(r, freshFilterMember(scanRef)) // merges r into l

	if m.MergeCount() != 1 {
		t.Fatalf("expected 1 merge, got %d", m.MergeCount())
	}
	// A queued task holding the loser pointer must still see the member.
	if !r.ContainsExactly(extra) {
		t.Fatal("forwarded loser must report ContainsExactly(extra) via the survivor — in-flight task would be stranded")
	}
	if !l.ContainsExactly(extra) {
		t.Fatal("survivor must hold the loser's distinct member")
	}
}

// TestMemoMerge_RecursiveUpwardReMerge is the Graefe-#1 regression: when
// two groups merge, parents that become equivalent must themselves merge
// (the paper's bottom-up recursion).
func TestMemoMerge_RecursiveUpwardReMerge(t *testing.T) {
	t.Parallel()
	scanRef := expressions.InitialOf(fixtureScan("T"))
	l := filterOver(scanRef)
	r := filterOver(scanRef)
	p1 := filterOver(l) // Filter(TRUE, →L)
	p2 := filterOver(r) // Filter(TRUE, →R) — equal to p1 once L≡R

	m := NewMemo(nil)
	m.RegisterReference(p1) // indexes p1, l, scanRef
	m.RegisterReference(p2) // indexes p2, r

	m.Integrate(r, freshFilterMember(scanRef)) // L≡R merge → cascades to P1≡P2

	if m.MergeCount() != 2 {
		t.Fatalf("expected 2 merges (L/R then P1/P2), got %d", m.MergeCount())
	}
	if l.Canonical() != r.Canonical() {
		t.Fatal("L and R must share a survivor")
	}
	if p1.Canonical() != p2.Canonical() {
		t.Fatal("parents P1 and P2 must merge via the upward worklist")
	}
}

// TestMemoMerge_SkipsCyclicMerge guards against the stack-overflow bug:
// merging a group with its own descendant would create a self-referential
// member. Such ancestor/descendant equivalences (idempotence rewrites like
// Distinct(Distinct(x))) must be skipped, leaving the groups separate.
func TestMemoMerge_SkipsCyclicMerge(t *testing.T) {
	t.Parallel()
	scanRef := expressions.InitialOf(fixtureScan("T"))
	inner := expressions.InitialOf(expressions.NewLogicalDistinctExpression(expressions.ForEachQuantifier(scanRef)))
	// outer ranges over inner: outer is an ANCESTOR of inner.
	outer := expressions.InitialOf(expressions.NewLogicalDistinctExpression(expressions.ForEachQuantifier(inner)))

	m := NewMemo(nil)
	m.RegisterReference(outer) // indexes outer, inner, scanRef

	// Simulate DistinctMerge yielding into outer an expr equal to inner's
	// member: Distinct(→scanRef). findEquivalentRef would match inner, but
	// inner is outer's descendant — merging would create a cycle.
	yielded := expressions.NewLogicalDistinctExpression(expressions.ForEachQuantifier(scanRef))
	m.Integrate(outer, yielded)

	if m.MergeCount() != 0 {
		t.Fatalf("cyclic (ancestor/descendant) merge must be skipped, got %d merges", m.MergeCount())
	}
	if outer.Canonical() == inner.Canonical() {
		t.Fatal("ancestor and descendant must remain distinct groups")
	}
}

// TestMemoMerge_SkipsWhenWinnersPresent is the §0 scope guard: a Reference
// carrying PLANNING-phase bookkeeping is never merged (its embedded raw
// References would not be canonicalized).
func TestMemoMerge_SkipsWhenWinnersPresent(t *testing.T) {
	t.Parallel()
	scanRef := expressions.InitialOf(fixtureScan("T"))
	l := filterOver(scanRef)
	r := filterOver(scanRef)
	r.SetWinner(expressions.NoProperties, fixtureScan("W")) // r is "optimized"

	m := NewMemo(nil)
	m.RegisterReference(l)
	m.RegisterReference(r)
	m.Integrate(r, freshFilterMember(scanRef))

	if m.MergeCount() != 0 {
		t.Fatalf("merge must be skipped when a group carries a winner, got %d", m.MergeCount())
	}
}

// TestMemoMerge_Deterministic pins the determinism invariant: the survivor
// is always the lower-id Reference regardless of run, and a merge fires
// every time.
func TestMemoMerge_Deterministic(t *testing.T) {
	t.Parallel()
	for i := 0; i < 50; i++ {
		scanRef := expressions.InitialOf(fixtureScan("T"))
		l := filterOver(scanRef)
		r := filterOver(scanRef)
		m := NewMemo(nil)
		m.RegisterReference(l) // registered first ⇒ lower id ⇒ survivor
		m.RegisterReference(r)
		m.Integrate(r, freshFilterMember(scanRef))
		if m.MergeCount() != 1 {
			t.Fatalf("run %d: expected 1 merge, got %d", i, m.MergeCount())
		}
		if l.Canonical() != l || r.Canonical() != l {
			t.Fatalf("run %d: survivor must deterministically be l (lower id)", i)
		}
	}
}

// TestMemoMerge_FiresThroughRealPlanner is the no-fake-checkbox proof: a
// real Planner run over a real expression tree triggers a cross-group
// merge. Branch A = Filter(P, Distinct(scan)); branch B = Distinct(Filter
// (P, scan)). PushFilterThroughDistinctRule rewrites A into B's form;
// because the inner Filter(P, scan) interns to B's existing Reference, A's
// yielded Distinct becomes structurally equal to B's member, and the two
// sibling union branches merge.
func TestMemoMerge_FiresThroughRealPlanner(t *testing.T) {
	t.Parallel()
	pred := predicates.NewValuePredicate(&values.FieldValue{Field: "active", Typ: values.TypeBool})

	scanRef := expressions.InitialOf(fixtureScan("T"))

	// Branch A: Filter(P, Distinct(scan))
	distinctRef := expressions.InitialOf(
		expressions.NewLogicalDistinctExpression(expressions.ForEachQuantifier(scanRef)))
	aRef := expressions.InitialOf(
		expressions.NewLogicalFilterExpression(
			[]predicates.QueryPredicate{pred}, expressions.ForEachQuantifier(distinctRef)))

	// Branch B: Distinct(Filter(P, scan))
	innerFilterRef := expressions.InitialOf(
		expressions.NewLogicalFilterExpression(
			[]predicates.QueryPredicate{pred}, expressions.ForEachQuantifier(scanRef)))
	bRef := expressions.InitialOf(
		expressions.NewLogicalDistinctExpression(expressions.ForEachQuantifier(innerFilterRef)))

	union := expressions.InitialOf(
		expressions.NewLogicalUnionExpression([]expressions.Quantifier{
			expressions.ForEachQuantifier(aRef),
			expressions.ForEachQuantifier(bRef),
		}))

	p := NewPlanner(DefaultExpressionRules(), nil)
	_, conv := p.Explore(union)
	if !conv {
		t.Fatal("planner did not converge")
	}
	if got := p.Memo().MergeCount(); got == 0 {
		t.Fatal("real planner run produced ZERO cross-group merges — the optimization never fired")
	}
	// The two equivalent branches must now share one canonical group.
	if aRef.Canonical() != bRef.Canonical() {
		t.Fatal("branches A and B rewrote to the same form but did not merge")
	}
}

// TestMemoMerge_MemoizeExpressionCanonicalAfterMerge pins the canonical
// compare in ExpressionRuleCall.MemoizeExpression (expression_rule_call.go):
// after a merge, the rule-call's Reference may be a forwarded loser while
// MemoizeExpression resolves an equal expr to the winner. The self-reference
// guard must compare CANONICAL identities, so it still fires (returns a fresh
// InitialOf) rather than handing back the winner and creating a cycle.
func TestMemoMerge_MemoizeExpressionCanonicalAfterMerge(t *testing.T) {
	t.Parallel()
	winner := expressions.InitialOf(fixtureScan("T"))
	loser := expressions.InitialOf(fixtureScan("T")) // structurally equal
	m := NewMemo(nil)
	m.RegisterReference(winner)
	winner.Absorb(loser) // loser now forwards to winner

	// Rule call firing "into" the (now forwarded) loser.
	c := &ExpressionRuleCall{Reference: loser, memo: m}
	// MemoizeExpression(equal scan) resolves to winner (leaf interning);
	// canonical(winner) == canonical(loser) == winner, so the guard fires.
	got := c.MemoizeExpression(fixtureScan("T"))

	if got == winner {
		t.Fatal("self-reference guard failed: returned the winner the rule is yielding into (would cycle)")
	}
	if got.Canonical() == loser.Canonical() {
		t.Fatal("returned a Reference canonical-equal to the rule-call's own group")
	}
}

// TestMemoMerge_CorrelatedToInvariant pins the Torvalds-#2 invariant:
// equivalent groups have equal correlation sets, so the survivor's
// GetCorrelatedTo is unchanged by the merge (the cache is invalidated and
// recomputes to the same value).
func TestMemoMerge_CorrelatedToInvariant(t *testing.T) {
	t.Parallel()
	scanRef := expressions.InitialOf(fixtureScan("T"))
	l := filterOver(scanRef)
	r := filterOver(scanRef)
	before := len(l.GetCorrelatedTo()) // populate the survivor cache

	m := NewMemo(nil)
	m.RegisterReference(l)
	m.RegisterReference(r)
	m.Integrate(r, freshFilterMember(scanRef))

	if got := len(l.GetCorrelatedTo()); got != before {
		t.Fatalf("survivor correlation set changed across a valid merge: before=%d after=%d", before, got)
	}
	if len(r.GetCorrelatedTo()) != before {
		t.Fatal("forwarded loser must report the survivor's correlation set")
	}
}
