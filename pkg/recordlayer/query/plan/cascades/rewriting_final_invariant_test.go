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
	task.Run(plannerTestContext(), p)

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

// TestOptimizeGroup_CoherencePhaseGateAndDetection completes the coherence
// instrument's pins: (a) the PLANNING arm records NOTHING even over a
// multi-final group (the check is REWRITING-gated — PLANNING's
// winner-per-ordering retention is legitimate); (b) the detection wiring is
// LIVE — a poisoned designation cache (stale entry at the current
// generation, the exact bug class the instrument canaries) is reported as a
// violation. Without (b), gutting the check entirely would leave every
// coherence test green.
func TestOptimizeGroup_CoherencePhaseGateAndDetection(t *testing.T) {
	t.Parallel()

	newScan := func() expressions.RelationalExpression {
		return expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	}
	build := func() *expressions.Reference {
		inner := expressions.InitialOf(newScan())
		inner.InsertFinal(newScan())
		ref := expressions.InitialOf(newScan())
		ref.InsertFinal(rfc186SortOver(inner))
		ref.InsertFinal(newScan())
		return ref
	}

	t.Run("PLANNING arm records nothing", func(t *testing.T) {
		t.Parallel()
		ref := build()
		p := NewPlanner(nil, nil)
		p.constraintMap = NewConstraintMap()
		p.dscope = newDesignationScope()
		p.SetVerifyRewritingCoherence(true)
		task := &OptimizeGroupTask{Phase: PhasePlanning, Ref: ref}
		task.Run(plannerTestContext(), p)
		if v := p.RewritingCoherenceViolations(); len(v) != 0 {
			t.Fatalf("PLANNING OptimizeGroup must never trip the REWRITING coherence check, got: %v", v)
		}
	})

	t.Run("poisoned designation cache is detected", func(t *testing.T) {
		t.Parallel()
		// finalsGeneration is process-global: a concurrent test's bump inside
		// the poison→check window evicts the injected entry and the check
		// legitimately recomputes — retry on that interference instead of
		// flaking (the sibling Run-order subtest's builds alone can trigger
		// it).
		for attempt := 0; ; attempt++ {
			ref := build()
			p := NewPlanner(nil, nil)
			p.constraintMap = NewConstraintMap()
			p.dscope = newDesignationScope()
			p.SetVerifyRewritingCoherence(true)

			task := &OptimizeGroupTask{Phase: PhaseRewriting, Ref: ref}
			task.Run(plannerTestContext(), p)
			if v := p.RewritingCoherenceViolations(); len(v) != 0 {
				t.Fatalf("healthy run must be coherent, got: %v", v)
			}
			// Simulate the staleness bug class the instrument canaries: a cache
			// entry that SURVIVED at the current generation while naming a
			// different expression (i.e. a mutation that failed to bump, or a
			// comparator bypassing the scope). The check must report it.
			impostor := rfc186SortOver(expressions.InitialOf(newScan()))
			gen := expressions.FinalsGeneration()
			p.dscope.cache[ref.Canonical()] = designationEntry{expr: impostor, gen: gen}
			p.checkRewritingCoherence(ref, ref.Winner())
			if len(p.RewritingCoherenceViolations()) > 0 {
				return // the stale entry was reported — detection wiring is live
			}
			if expressions.FinalsGeneration() != gen && attempt < 20 {
				continue // external bump evicted the poison mid-window — retry
			}
			t.Fatal("a stale designation surviving at the current generation must be reported — the instrument's detection wiring is dead")
		}
	})

	t.Run("poisoned cache detected through Run's own call order", func(t *testing.T) {
		t.Parallel()
		// The subtest above proves the DETECTION wiring by calling
		// checkRewritingCoherence directly; this one pins the CALL ORDER
		// inside OptimizeGroupTask.Run. The check must run BEFORE
		// PruneToSet: pruning bumps the finals generation, which evicts a
		// poisoned entry and recomputes against the post-prune singleton —
		// so under the old post-prune ordering this poison is silently
		// laundered and Run reports nothing. Only the pre-prune ordering
		// catches it through the real path.
		//
		// finalsGeneration is process-global, so a concurrent test's bump
		// between poisoning and the check would ALSO evict the entry —
		// retry on that interference instead of flaking.
		for attempt := 0; ; attempt++ {
			ref := build()
			p := NewPlanner(nil, nil)
			p.constraintMap = NewConstraintMap()
			p.dscope = newDesignationScope()
			p.SetVerifyRewritingCoherence(true)
			impostor := rfc186SortOver(expressions.InitialOf(newScan()))
			gen := expressions.FinalsGeneration()
			p.dscope.cache[ref.Canonical()] = designationEntry{expr: impostor, gen: gen}
			task := &OptimizeGroupTask{Phase: PhaseRewriting, Ref: ref}
			task.Run(plannerTestContext(), p)
			if len(p.RewritingCoherenceViolations()) > 0 {
				return // detected through Run — the pre-prune ordering is live
			}
			if expressions.FinalsGeneration() != gen+1 && attempt < 20 {
				continue // external bump evicted the poison mid-run — retry
			}
			t.Fatal("a poisoned designation must be detected by OptimizeGroupTask.Run itself — the coherence check no longer runs against the pre-prune candidate set")
		}
	})
}

// TestDesignatedFinal_NoCacheInUnfinalizedWindow pins the review-required
// no-cache rule for the exploratory fallback: the finals generation does
// not observe exploratory growth, so a members-derived designation must be
// recomputed per request — caching it would freeze the first answer across
// later exploratory inserts (history dependence in the un-finalized
// window).
func TestDesignatedFinal_NoCacheInUnfinalizedWindow(t *testing.T) {
	t.Parallel()
	inner := expressions.InitialOf(rfc186Scan("T"))
	inner.InsertFinal(rfc186Scan("T"))
	ref := expressions.InitialOf(rfc186SortOver(inner)) // exploratory-only, deeper tree

	s := newDesignationScope()
	first := s.designated(ref, nil)
	if first == nil {
		t.Fatal("exploratory fallback must designate")
	}
	// Exploratory growth: a strictly better (shallower) member arrives —
	// WITHOUT any finals-generation bump.
	ref.Insert(rfc186Scan("T"))
	second := s.designated(ref, nil)
	if _, isScan := second.(*expressions.FullUnorderedScanExpression); !isScan {
		t.Fatalf("members-derived designation must be recomputed per request (got %T) — a cached fallback froze the pre-growth answer", second)
	}
}

// TestDesignatedFinal_BackEdgeTaintNotCached pins the taint rule: a
// designation whose computation transitively consumed a back-edge ranking
// is valid only under that traversal and must not be cached — otherwise a
// recursive-CTE group's cached designation depends on which parent reached
// it first.
func TestDesignatedFinal_BackEdgeTaintNotCached(t *testing.T) {
	t.Parallel()
	// child holds TWO finals (a ranking is required — an argmin-of-one
	// consumes no back-edge and may cache), one of which ranges back over
	// ref; ref's final ranges over child — designating ref consumes
	// child's RANKED designation, whose comparison hits the ref back-edge.
	ref := expressions.InitialOf(rfc186Scan("T"))
	child := expressions.InitialOf(rfc186Scan("T"))
	child.InsertFinal(expressions.NewLogicalSortExpression(nil, expressions.ForEachQuantifier(ref)))
	child.InsertFinal(rfc186Scan("T"))
	ref.InsertFinal(expressions.NewLogicalSortExpression(nil, expressions.ForEachQuantifier(child)))
	ref.InsertFinal(rfc186Scan("T"))

	s := newDesignationScope()
	_ = s.designated(ref, nil)
	if _, cached := s.cache[child.Canonical()]; cached {
		t.Fatal("a back-edge-tainted designation must not be cached (valid only under its visiting set)")
	}
	if _, cached := s.cache[ref.Canonical()]; cached {
		t.Fatal("the computation that CONSUMED a tainted child designation must not cache either")
	}
}

// rfc186SelectOver wraps a child reference in a single-quantifier
// SelectExpression, with the quantifier supplied by the caller so edge
// attributes (null-on-empty, kind) can vary while the child content stays
// identical.
func rfc186SelectOver(q expressions.Quantifier) expressions.RelationalExpression {
	return expressions.NewSelectExpression(q.GetFlowedObjectValue(), []expressions.Quantifier{q}, nil)
}

// TestDesignatedFinal_ExploratoryChildTaintNotCached pins the ancestor half
// of the no-cache-unfinalized rule: a designation that TRANSITIVELY consumed
// an unfinalized child's members-derived answer must not be cached either.
// The generation key observes final-set mutations only — a later exploratory
// Insert on the child changes the child's answer WITHOUT a bump, so a cached
// ancestor would keep serving the pre-growth derivation while a fresh scope
// (and OptimizeGroup's compare loop) ranks with the post-growth one; the
// pre-prune coherence check then reports the mismatch and fails a valid
// plan.
func TestDesignatedFinal_ExploratoryChildTaintNotCached(t *testing.T) {
	t.Parallel()
	// finalsGeneration is process-global: a concurrent test's bump between
	// the warming lookup and the post-growth lookup would evict a (buggy)
	// cached ancestor and mask the regression — retry on interference so the
	// pin is deterministic in both directions.
	for attempt := 0; ; attempt++ {
		// child: UNFINALIZED, holding one deep member (2 nested selects).
		inner := expressions.InitialOf(rfc186Scan("T"))
		innerSel := expressions.InitialOf(rfc186SelectOver(expressions.ForEachQuantifier(inner)))
		child := expressions.InitialOf(rfc186SelectOver(expressions.ForEachQuantifier(innerSel)))

		// parent finals: fA = select over the unfinalized child (select-count
		// tracks the child's designated member: 3 now, 1 after growth);
		// fB = select over a FINALIZED 1-select tree (constant count 2).
		fA := rfc186SelectOver(expressions.ForEachQuantifier(child))
		fbInner := expressions.InitialOf(rfc186SelectOver(expressions.ForEachQuantifier(expressions.InitialOf(rfc186Scan("U")))))
		fbInner.InsertFinal(rfc186SelectOver(expressions.ForEachQuantifier(expressions.InitialOf(rfc186Scan("U")))))
		fB := rfc186SelectOver(expressions.ForEachQuantifier(fbInner))
		parent := expressions.InitialOf(fA)
		parent.InsertFinal(fA)
		parent.InsertFinal(fB)

		s := newDesignationScope()
		gen := expressions.FinalsGeneration()
		first := s.designated(parent, nil)
		if first != fB {
			t.Fatalf("precondition: with the deep child, fB (constant 2 selects) must out-rank fA (3), got %T", first)
		}

		// Exploratory growth on the child: a bare scan (0 selects) becomes
		// its members-best — NO finals-generation bump.
		child.Insert(rfc186Scan("T"))

		second := s.designated(parent, nil)
		fresh := newDesignationScope().designated(parent, nil)
		if fresh != fA {
			t.Fatalf("precondition: after growth a fresh scope must rank fA (1 select) over fB (2), got %T", fresh)
		}
		if second != fresh {
			t.Fatal("stale designation served from cache after exploratory child growth — the ancestor consumed an unfinalized child's answer and must not have been cached (exploratory taint)")
		}
		if expressions.FinalsGeneration() == gen || attempt >= 20 {
			return // clean window: the scope genuinely re-derived through the grown child
		}
		// An external generation bump would ALSO have evicted a (buggy)
		// cached ancestor, making this pass prove nothing — resample until a
		// clean window is observed.
	}
}

// TestDesignatedFinal_AttributeVariantTieBreak pins the deep-hash tier
// against the refined memo identity: two finals differing ONLY in a
// quantifier's null-on-empty flag (a LEFT box and its INNER twin over the
// SAME child) tie through tiers 1-4, so the deep-hash tie-break must see
// the edge attribute — otherwise the designation flips with insertion
// order, and with it every derived cost property (which rules fire, which
// twin PartitionBinarySelectRule sees).
func TestDesignatedFinal_AttributeVariantTieBreak(t *testing.T) {
	t.Parallel()
	build := func(noeFirst bool) *expressions.Reference {
		child := expressions.InitialOf(rfc186Scan("T"))
		child.InsertFinal(rfc186Scan("T"))
		alias := values.NamedCorrelationIdentifier("q")
		plain := rfc186SelectOver(expressions.NamedForEachQuantifier(alias, child))
		noe := rfc186SelectOver(expressions.NamedForEachNullOnEmptyQuantifier(alias, child))
		ref := expressions.InitialOf(plain)
		if noeFirst {
			ref.InsertFinal(noe)
			ref.InsertFinal(plain)
		} else {
			ref.InsertFinal(plain)
			ref.InsertFinal(noe)
		}
		return ref
	}
	pick := func(ref *expressions.Reference) bool {
		d := newDesignationScope().designated(ref, nil)
		sel, ok := d.(*expressions.SelectExpression)
		if !ok {
			t.Fatalf("designated must be a select, got %T", d)
		}
		return sel.GetQuantifiers()[0].IsNullOnEmpty()
	}
	a := pick(build(false))
	b := pick(build(true))
	if a != b {
		t.Fatalf("designation flipped with insertion order (noe=%v vs noe=%v) — the deep-hash tie-break is blind to quantifier attributes", a, b)
	}
}
