package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func rfc186Scan(table string) expressions.RelationalExpression {
	return expressions.NewFullUnorderedScanExpression([]string{table}, values.UnknownType)
}

// rfc186SortOver wraps a child reference in a LogicalSortExpression — a
// distinct canonical alternative (one extra tree node) for designation
// ranking.
func rfc186SortOver(child *expressions.Reference) expressions.RelationalExpression {
	return expressions.NewLogicalSortExpression(nil, expressions.ForEachQuantifier(child))
}

// TestDesignatedFinal_InsertionOrderPermutation is RFC-186's class-level
// pin (review condition): the designation — and therefore every derived
// cost property — is a function of the candidate SET's content, never of
// insertion history. Two references holding the same two canonical
// alternatives in OPPOSITE insertion order must designate the same-shaped
// expression and compare identically against a common competitor.
func TestDesignatedFinal_InsertionOrderPermutation(t *testing.T) {
	t.Parallel()

	build := func(scanFirst bool) *expressions.Reference {
		inner := expressions.InitialOf(rfc186Scan("T"))
		inner.InsertFinal(rfc186Scan("T"))
		ref := expressions.InitialOf(rfc186Scan("T"))
		a := rfc186Scan("T")       // plain scan: 1 node, no selects
		b := rfc186SortOver(inner) // sort-over-scan: deeper tree
		if scanFirst {
			ref.InsertFinal(a)
			ref.InsertFinal(b)
		} else {
			ref.InsertFinal(b)
			ref.InsertFinal(a)
		}
		return ref
	}

	s := newDesignationScope()
	d1 := s.designated(build(true), nil)
	d2 := s.designated(build(false), nil)
	if d1 == nil || d2 == nil {
		t.Fatal("designation must exist for a non-empty final set")
	}
	// Same-shaped designation regardless of insertion order: compare the
	// designated trees under the designated comparator — 0 means the
	// choice is content-identical.
	if cmp := s.compare(d1, d2, nil); cmp != 0 {
		t.Fatalf("designation depends on insertion order: %T vs %T (cmp=%d)", d1, d2, cmp)
	}
}

// TestDesignatedFinal_GenerationInvalidation pins the generation-keyed
// cache (review condition 1): a designation consumed BEFORE a final-set
// growth must not survive the growth — the next lookup re-designates
// against the new set (first-request-wins caching would re-import history
// dependence).
func TestDesignatedFinal_GenerationInvalidation(t *testing.T) {
	t.Parallel()

	inner := expressions.InitialOf(rfc186Scan("T"))
	inner.InsertFinal(rfc186Scan("T"))
	ref := expressions.InitialOf(rfc186Scan("T"))
	ref.InsertFinal(rfc186SortOver(inner)) // deeper tree first

	s := newDesignationScope()
	first := s.designated(ref, nil)
	if first == nil {
		t.Fatal("first designation must exist")
	}
	// Growth: a strictly better (shallower) candidate arrives.
	ref.InsertFinal(rfc186Scan("T"))
	second := s.designated(ref, nil)
	if _, isScan := second.(*expressions.FullUnorderedScanExpression); !isScan {
		t.Fatalf("designation not refreshed after final growth: got %T, want the newly-inserted plain scan", second)
	}
}

// TestDesignatedFinal_CycleGuard pins the recursion bottom-out (review
// condition 2): a reference reachable from its own designation walk (the
// recursive-CTE back-edge shape) must terminate and designate
// deterministically.
func TestDesignatedFinal_CycleGuard(t *testing.T) {
	t.Parallel()

	ref := expressions.InitialOf(rfc186Scan("T"))
	// Back-edge: a final whose quantifier ranges over ref itself.
	ref.InsertFinal(expressions.NewLogicalSortExpression(nil, expressions.ForEachQuantifier(ref)))
	ref.InsertFinal(rfc186Scan("T"))

	s := newDesignationScope()
	d := s.designated(ref, nil)
	if d == nil {
		t.Fatal("cycle guard must still produce a designation")
	}
	// Deterministic across repeated evaluation.
	if d2 := s.designated(ref, nil); d2 != d {
		t.Fatal("cycle-guarded designation must be stable")
	}
}

// TestOptimizeGroup_RewritingCoherence pins the re-specified RFC-186
// instrument: after a REWRITING OptimizeGroup stamps its winner, the
// group's designation IS that winner (same comparator ⇒ same choice), and
// the check records nothing. The PLANNING arm must record nothing
// regardless (multi-final retention is legitimate there).
func TestOptimizeGroup_RewritingCoherence(t *testing.T) {
	t.Parallel()

	inner := expressions.InitialOf(rfc186Scan("T"))
	inner.InsertFinal(rfc186Scan("T"))
	ref := expressions.InitialOf(rfc186Scan("T"))
	ref.InsertFinal(rfc186SortOver(inner))
	ref.InsertFinal(rfc186Scan("T"))

	p := NewPlanner(nil, nil)
	p.constraintMap = NewConstraintMap()
	p.dscope = newDesignationScope()
	p.SetVerifyRewritingCoherence(true)

	task := &OptimizeGroupTask{Phase: PhaseRewriting, Ref: ref}
	task.Run(p)

	if v := p.RewritingCoherenceViolations(); len(v) != 0 {
		t.Fatalf("winner and designation come from the same comparator — coherence must hold, got: %v", v)
	}
	if w := ref.Winner(); w == nil {
		t.Fatal("REWRITING OptimizeGroup must stamp a winner")
	} else if _, isScan := w.(*expressions.FullUnorderedScanExpression); !isScan {
		t.Fatalf("the shallower plain scan must win the rewriting compare, got %T", w)
	}
	if got := len(ref.FinalMembers()); got != 1 {
		t.Fatalf("REWRITING OptimizeGroup prunes ITS OWN group to the winner, got %d finals", got)
	}
}
