package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
)

// RFC-186 §2A: the VIRTUAL PRUNE. REWRITING cost-property derivation must be
// a function of a chosen candidate tree — Java achieves that by physically
// pruning every child reference to one final expression before parents
// compare (ExpressionCountProperty.forReference: Verify(size()==1) +
// getOnlyElement). Go cannot adopt the physical prune: PLANNING re-derivation
// parity is missing, and the forced prune was tried and reverted (the
// RFC-153 buried-leg and cross-join-EXISTS shapes lost their implementable
// form — see ExploreGroupTask's stage-boundary comment). Instead, every
// REWRITING property derivation traverses each child reference's DESIGNATED
// final: the expression the prune WOULD have kept, chosen with the same
// comparator OptimizeGroup uses, memoized generation-keyed, nothing pruned.
// Costing sees Java's post-prune world; the stage boundary keeps the
// alternatives PLANNING needs. When PLANNING re-derivation parity lands, the
// designation degenerates to the single final and Java's Verify(==1) becomes
// enforceable (DIVERGENCES.md: virtual vs physical prune).

// designationEntry caches a reference's designated final at a finals
// generation. ANY final-set mutation anywhere bumps the global generation
// (expressions.FinalsGeneration) and invalidates the entry — a conservative
// over-approximation of the reachable-subtree generation vector: it
// invalidates more often, never less, so a stale designation is
// unrepresentable (first-request-wins caching would re-import exactly the
// history dependence RFC-186 exists to kill).
type designationEntry struct {
	expr expressions.RelationalExpression
	gen  uint64
}

// designationScope is one designation cache + the designated five-tier
// REWRITING comparator. The planner owns one per planning run (OptimizeGroup
// coherence: winner and designation come from the SAME comparator); the
// package-level RewritingCostModelLess mints a fresh scope per call —
// identical semantics, merely uncached.
type designationScope struct {
	cache map[*expressions.Reference]designationEntry
	// backEdgeHits counts cycle-guard firings within this scope. A
	// designation whose computation CONSUMED a back-edge ranking — its own
	// or transitively through any child designation — is valid only under
	// that visiting set and must not be cached: a cached
	// tainted answer would make a recursive-CTE group's designation depend
	// on which parent reached it first (query-order history dependence,
	// the disease this file exists to kill). Single-threaded per scope, so
	// snapshot-before/compare-after brackets a computation exactly.
	backEdgeHits int
	// exploratoryHits counts members-fallback designations within this
	// scope. The generation counter observes FINAL-set mutations only, so a
	// designation that transitively consumed an UNFINALIZED child's
	// members-derived answer is not protected by the generation key: a later
	// exploratory Insert on that child changes the child's answer WITHOUT a
	// bump, and a cached ancestor would serve the stale derivation — the
	// pre-prune coherence check then reports a mismatch against the fresh
	// OptimizeGroup ranking and fails valid plans. Same bracket discipline
	// as backEdgeHits: any ancestor whose computation consumed a fallback
	// stays uncached until the child finalizes (that InsertFinal bumps the
	// generation and the cached tier takes over).
	exploratoryHits int
}

func newDesignationScope() *designationScope {
	return &designationScope{cache: map[*expressions.Reference]designationEntry{}}
}

// designated returns ref's designated final expression: argmin over the
// final members under the designated comparator (semantic-hash tie-break
// inside the comparator). A reference with NO finals yet (its
// FinalizeExpressionsRule has not fired) designates from the exploratory
// members instead — still content-deterministic, and strictly better than
// the pre-RFC-186 behaviour of not seeing the subtree at all. Returns nil
// only for an empty reference.
//
// visiting is the cycle guard (a recursive-CTE back-edge reaches this
// walk): on a revisit the candidate set is ranked WITHOUT recursion, by
// node-content hash only — conservative and deterministic.
func (s *designationScope) designated(ref *expressions.Reference, visiting map[*expressions.Reference]bool) expressions.RelationalExpression {
	if ref == nil {
		return nil
	}
	ref = ref.Canonical()
	gen := expressions.FinalsGeneration()
	if e, ok := s.cache[ref]; ok && e.gen == gen {
		return e.expr
	}
	candidates := ref.FinalMembers()
	fromFinals := len(candidates) > 0
	if !fromFinals {
		candidates = ref.Members()
		// Taint every enclosing computation: an ancestor that consumes this
		// members-derived answer must not cache (see exploratoryHits).
		s.exploratoryHits++
	}
	if len(candidates) == 0 {
		return nil
	}
	// Cycle safety lives in the WALKERS, not here: descendDesignated treats
	// an in-visiting reference as a traversal leaf (and taints the
	// computation), so ranking with the full comparator is cycle-safe and
	// this function is never entered for a reference already in visiting —
	// its only in-visiting caller is descendDesignated, which leafs first.
	var best expressions.RelationalExpression
	if visiting == nil {
		visiting = map[*expressions.Reference]bool{}
	}
	hitsBefore := s.backEdgeHits
	exHitsBefore := s.exploratoryHits
	visiting[ref] = true
	for _, c := range candidates {
		if best == nil || s.compare(c, best, visiting) < 0 {
			best = c
		}
	}
	delete(visiting, ref)
	if fromFinals && s.backEdgeHits == hitsBefore && s.exploratoryHits == exHitsBefore {
		s.cache[ref] = designationEntry{expr: best, gen: gen}
	}
	// !fromFinals is deliberately NOT cached: the generation counter bumps
	// on FINAL-set mutations only — exploratory Insert does not bump it, so
	// a cached members-derived designation would survive later exploratory
	// growth at the same generation and re-import insertion-order
	// dependence in exactly the un-finalized-child window the fallback
	// serves. Recomputed per request until the group finalizes (the
	// finalization insert bumps the generation and the cached tier takes
	// over).
	return best
}

// compare is the designated five-tier REWRITING comparator — the tail of
// Java's RewritingCostModel.compare(), with every property derived through
// designated child finals (Java derives through the pruned single final;
// see RewritingCostModelLess for the documented outerJoinCount omission):
//  1. Fewer SelectExpressions
//  2. Fewer TableFunctionExpressions
//  3. Fewer normalized residual predicate conjuncts (CNF full-size)
//  4. More predicates at deeper levels (push predicates down)
//  5. Designated deep-hash tiebreak
func (s *designationScope) compare(a, b expressions.RelationalExpression, visiting map[*expressions.Reference]bool) int {
	if visiting == nil {
		visiting = map[*expressions.Reference]bool{}
	}
	selectsA := s.exprCount(a, isSelectExpression, visiting)
	selectsB := s.exprCount(b, isSelectExpression, visiting)
	if selectsA != selectsB {
		return intCompare(selectsA, selectsB)
	}

	tfA := s.exprCount(a, isTableFunctionExpression, visiting)
	tfB := s.exprCount(b, isTableFunctionExpression, visiting)
	if tfA != tfB {
		return intCompare(tfA, tfB)
	}

	conjA := s.residualConjuncts(a, visiting)
	conjB := s.residualConjuncts(b, visiting)
	if conjA != conjB {
		return intCompare(conjA, conjB)
	}

	infoA := map[int]int{}
	s.predCountByLevel(a, infoA, visiting)
	infoB := map[int]int{}
	s.predCountByLevel(b, infoB, visiting)
	if cmp := comparePredicateCountByLevel(infoB, infoA); cmp != 0 {
		return cmp
	}

	hashA := s.deepHash(a, visiting)
	hashB := s.deepHash(b, visiting)
	if hashA != hashB {
		if hashA < hashB {
			return -1
		}
		return 1
	}
	return 0
}

// descendDesignated resolves a child reference's designated final and, when
// the descent is admissible, invokes walk over it with the reference marked
// in visiting for the walk's duration. A reference already in visiting is a
// TRAVERSAL LEAF: without this every property walker would re-enter a
// recursive-CTE back-edge's designated tree unboundedly (the visiting guard
// on designated() protects only candidate RANKING — the cache is consulted
// first, so a cached cyclic designation would recurse straight past it to a
// fatal stack overflow). Marking during the descent also makes deepHash
// cycle-safe as the back-edge tie-break.
func (s *designationScope) descendDesignated(ref *expressions.Reference, visiting map[*expressions.Reference]bool, walk func(expressions.RelationalExpression)) {
	if ref == nil {
		return
	}
	cref := ref.Canonical()
	if visiting[cref] {
		// Back-edge: opaque leaf. This is THE cycle event — the walk's
		// result now depends on which ancestors were in flight, so the
		// computation is tainted and must not be cached (see backEdgeHits).
		s.backEdgeHits++
		return
	}
	child := s.designated(cref, visiting)
	if child == nil {
		return
	}
	visiting[cref] = true
	walk(child)
	delete(visiting, cref)
}

// exprCount counts tree nodes passing filter, recursing through each child
// reference's DESIGNATED final (Java ExpressionCountProperty.visitDefault +
// forReference-getOnlyElement, with designation as the virtual prune).
func (s *designationScope) exprCount(e expressions.RelationalExpression, filter func(expressions.RelationalExpression) bool, visiting map[*expressions.Reference]bool) int {
	if e == nil {
		return 0
	}
	count := 0
	if filter == nil || filter(e) {
		count = 1
	}
	for _, q := range e.GetQuantifiers() {
		s.descendDesignated(q.GetRangesOver(), visiting, func(child expressions.RelationalExpression) {
			count += s.exprCount(child, filter, visiting)
		})
	}
	return count
}

// residualConjuncts counts the CNF full-size of every predicate on the
// designated tree (Java NormalizedResidualPredicateProperty: the node's own
// predicates + recursion through the single final per child). Counts
// LOGICAL predicate carriers (RelationalExpressionWithPredicates) and the
// physical filter/NLJ nodes alike, so the tier is live on the all-logical
// REWRITING memo — the pre-RFC-186 physical-only descent counted nothing
// there.
func (s *designationScope) residualConjuncts(e expressions.RelationalExpression, visiting map[*expressions.Reference]bool) int {
	if e == nil {
		return 0
	}
	count := 0
	if wp, ok := e.(expressions.RelationalExpressionWithPredicates); ok {
		for _, p := range wp.GetPredicates() {
			count += int(cnfSize(p))
		}
	}
	for _, q := range e.GetQuantifiers() {
		s.descendDesignated(q.GetRangesOver(), visiting, func(child expressions.RelationalExpression) {
			count += s.residualConjuncts(child, visiting)
		})
	}
	return count
}

// predCountByLevel fills counts[level] with predicate counts by tree depth
// over the designated tree (level 0 = leaves; Java
// PredicateCountByLevelProperty). Returns the node's level.
func (s *designationScope) predCountByLevel(e expressions.RelationalExpression, counts map[int]int, visiting map[*expressions.Reference]bool) int {
	if e == nil {
		return -1
	}
	maxChildLevel := -1
	for _, q := range e.GetQuantifiers() {
		s.descendDesignated(q.GetRangesOver(), visiting, func(child expressions.RelationalExpression) {
			if lvl := s.predCountByLevel(child, counts, visiting); lvl > maxChildLevel {
				maxChildLevel = lvl
			}
		})
	}
	currentLevel := maxChildLevel + 1
	// This map is SPARSE (entries only at levels that carry predicates), unlike
	// Java's DENSE PredicateCountByLevelVisitor (which always puts a 0 entry for
	// a non-predicate level). comparePredicateCountByLevel walks the UNION of
	// levels, so it stays antisymmetric and matches Java's per-level counts on
	// sparse input; the ONLY residual divergence is the highest-level tiebreak,
	// which here is the highest PREDICATE level rather than Java's tree depth —
	// a pre-existing gap booked as Finding 6-followup (making this dense flips
	// REWRITING survivors and needs Java-verification of each).
	if wp, ok := e.(expressions.RelationalExpressionWithPredicates); ok {
		counts[currentLevel] += len(wp.GetPredicates())
	}
	return currentLevel
}

// deepHash hashes the designated tree — schema-neutral tie-break node content
// folded with each child edge's quantifier attributes and the designated
// children's hashes, order-sensitive (see deepHashCode's commutative-XOR
// caution). Memo identity remains independently schema-aware.
//
// The EDGE ATTRIBUTES (kind / null-on-empty / strict-single) must be folded:
// they live on the quantifier, so HashCodeWithoutChildren cannot see them and
// the child content is identical — two finals differing only in a
// quantifier's flag (a LEFT box vs its INNER twin, both retained by the
// refined memo identity) would otherwise tie through every comparator tier
// and leave the designation insertion-order dependent, the exact
// nondeterminism the virtual prune exists to kill. Folded regardless of
// whether the descent leafs at a back-edge — the edge itself is still part
// of the tree's identity.
func (s *designationScope) deepHash(e expressions.RelationalExpression, visiting map[*expressions.Reference]bool) uint64 {
	if e == nil {
		return 0
	}
	h := tieBreakNodeHash(e)
	for _, q := range e.GetQuantifiers() {
		// attrs == 0 is the default edge (plain ForEach): folded as a no-op
		// so attribute-default trees keep their exact pre-refinement hash —
		// only a variant edge perturbs. The guarantee needed is variant ≠
		// plain, which a one-sided fold provides.
		attrs := uint64(q.Kind()) << 2
		if q.IsNullOnEmpty() {
			attrs |= 1 << 1
		}
		if q.IsStrictSingle() {
			attrs |= 1
		}
		if attrs != 0 {
			h = h*0x100000001b3 ^ (attrs + 0x9e3779b97f4a7c15)
		}
		s.descendDesignated(q.GetRangesOver(), visiting, func(child expressions.RelationalExpression) {
			childHash := s.deepHash(child, visiting)
			h = h*0x100000001b3 ^ (childHash*0x517cc1b727220a95 + 0x6c62272e07bb0142)
		})
	}
	return h
}
