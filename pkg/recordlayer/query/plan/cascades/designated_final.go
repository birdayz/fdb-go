package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
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
// REWRITING comparator. The planner owns one per Plan() call (OptimizeGroup
// coherence: winner and designation come from the SAME comparator); the
// package-level RewritingCostModelLess mints a fresh scope per call —
// identical semantics, merely uncached.
type designationScope struct {
	cache map[*expressions.Reference]designationEntry
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
	}
	if len(candidates) == 0 {
		return nil
	}
	var best expressions.RelationalExpression
	if visiting[ref] {
		// Back-edge: rank by node-content hash only, no recursion.
		for _, c := range candidates {
			if best == nil || c.HashCodeWithoutChildren() < best.HashCodeWithoutChildren() {
				best = c
			}
		}
		// Do NOT cache a cycle-conservative answer — it is context-dependent
		// (valid only under this visiting set).
		return best
	}
	if visiting == nil {
		visiting = map[*expressions.Reference]bool{}
	}
	visiting[ref] = true
	for _, c := range candidates {
		if best == nil || s.compare(c, best, visiting) < 0 {
			best = c
		}
	}
	delete(visiting, ref)
	if fromFinals {
		// Exploratory-derived designations are transient (the group has not
		// finalized); caching them is sound (generation-keyed — the
		// finalization insert bumps the generation) but pointless only if
		// growth is imminent. Cache both: correctness comes from the key.
		s.cache[ref] = designationEntry{expr: best, gen: gen}
	} else {
		s.cache[ref] = designationEntry{expr: best, gen: gen}
	}
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
		if child := s.designated(q.GetRangesOver(), visiting); child != nil {
			count += s.exprCount(child, filter, visiting)
		}
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
			count += int(cnfSizeOfPredicate(p))
		}
	}
	for _, q := range e.GetQuantifiers() {
		if child := s.designated(q.GetRangesOver(), visiting); child != nil {
			count += s.residualConjuncts(child, visiting)
		}
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
		if child := s.designated(q.GetRangesOver(), visiting); child != nil {
			if lvl := s.predCountByLevel(child, counts, visiting); lvl > maxChildLevel {
				maxChildLevel = lvl
			}
		}
	}
	currentLevel := maxChildLevel + 1
	if wp, ok := e.(expressions.RelationalExpressionWithPredicates); ok {
		counts[currentLevel] += len(wp.GetPredicates())
	}
	return currentLevel
}

// deepHash hashes the designated tree — node content hash folded with the
// designated children's hashes, order-sensitive (see deepHashCode's
// commutative-XOR caution).
func (s *designationScope) deepHash(e expressions.RelationalExpression, visiting map[*expressions.Reference]bool) uint64 {
	if e == nil {
		return 0
	}
	h := e.HashCodeWithoutChildren()
	for _, q := range e.GetQuantifiers() {
		if child := s.designated(q.GetRangesOver(), visiting); child != nil {
			childHash := s.deepHash(child, visiting)
			h = h*0x100000001b3 ^ (childHash*0x517cc1b727220a95 + 0x6c62272e07bb0142)
		}
	}
	return h
}

// cnfSizeOfPredicate is cnfSize with the package's overflow-guarded
// estimator (rule_normalize_predicates.go).
func cnfSizeOfPredicate(p predicates.QueryPredicate) int64 { return cnfSize(p) }
