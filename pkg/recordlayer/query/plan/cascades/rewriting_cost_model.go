package cascades

import (
	"fmt"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
)

// rewritingComparator is Java's RewritingCostModel.compare over the tree a
// REWRITING final stands for. Every property descends through each child
// reference's ONE final member, as Java's do (ExpressionCountProperty.
// forReference: Verify(size() == 1) + getOnlyElement): REWRITING prunes every
// group it reaches to its winning final before a parent is costed
// (OptimizeInputs → OptimizeGroup over each final's inputs), so a parent's
// alternatives are compared over single-final children.
//
// A child with other than one final is a scheduling defect. The comparator
// treats it as an opaque leaf and records the first violation in err, which
// the planner's OptimizeGroup returns as a RewritingPruneError.
//
// clientTrees admits one more shape: a reference with no final and exactly
// one exploratory member, the form Go's InitialOf gives a client-built tree
// (Java's Reference.initialOf holds the expression as the one final at the
// INITIAL stage). The planner never compares such a child; the package-level
// RewritingCostModelLess, which compares trees outside a planning run, admits
// it.
type rewritingComparator struct {
	clientTrees bool
	err         error
}

// onlyFinal is the expression a child reference contributes to the tree.
func (c *rewritingComparator) onlyFinal(ref *expressions.Reference) (expressions.RelationalExpression, bool) {
	ref = ref.Canonical()
	finals := ref.FinalMembers()
	if len(finals) == 1 {
		return finals[0], true
	}
	if c.clientTrees && len(finals) == 0 {
		if members := ref.Members(); len(members) == 1 {
			return members[0], true
		}
	}
	if c.err == nil {
		c.err = &RewritingPruneError{Finals: len(finals)}
	}
	return nil, false
}

// descend walks a child reference's one final with the reference marked in
// visiting for the walk's duration. A reference already in visiting is a
// back-edge (a recursive-CTE group reaching itself) and an opaque leaf.
func (c *rewritingComparator) descend(ref *expressions.Reference, visiting map[*expressions.Reference]bool, walk func(expressions.RelationalExpression)) {
	if ref == nil {
		return
	}
	cref := ref.Canonical()
	if visiting[cref] {
		return
	}
	child, ok := c.onlyFinal(cref)
	if !ok {
		return
	}
	visiting[cref] = true
	walk(child)
	delete(visiting, cref)
}

// compare is Java's RewritingCostModel.compare():
//  0. Fewer LEFT OUTER selects (Java's outerJoinCount)
//  1. Fewer SelectExpressions
//  2. Fewer TableFunctionExpressions
//  3. Fewer normalized residual predicate conjuncts (CNF full-size)
//  4. More predicates at deeper levels (push predicates down)
//  5. Deep-hash tiebreak
func (c *rewritingComparator) compare(a, b expressions.RelationalExpression) int {
	visiting := map[*expressions.Reference]bool{}
	outerA := c.exprCount(a, isLeftOuterJoinSelect, visiting)
	outerB := c.exprCount(b, isLeftOuterJoinSelect, visiting)
	if outerA != outerB {
		return intCompare(outerA, outerB)
	}

	selectsA := c.exprCount(a, isSelectExpression, visiting)
	selectsB := c.exprCount(b, isSelectExpression, visiting)
	if selectsA != selectsB {
		return intCompare(selectsA, selectsB)
	}

	tfA := c.exprCount(a, isTableFunctionExpression, visiting)
	tfB := c.exprCount(b, isTableFunctionExpression, visiting)
	if tfA != tfB {
		return intCompare(tfA, tfB)
	}

	conjA := c.residualConjuncts(a, visiting)
	conjB := c.residualConjuncts(b, visiting)
	if conjA != conjB {
		return intCompare(conjA, conjB)
	}

	infoA := map[int]int{}
	c.predCountByLevel(a, infoA, visiting)
	infoB := map[int]int{}
	c.predCountByLevel(b, infoB, visiting)
	if cmp := comparePredicateCountByLevel(infoB, infoA); cmp != 0 {
		return cmp
	}

	hashA := c.deepHash(a, visiting)
	hashB := c.deepHash(b, visiting)
	if hashA != hashB {
		if hashA < hashB {
			return -1
		}
		return 1
	}
	return 0
}

// exprCount counts tree nodes passing filter (Java
// ExpressionCountProperty.visitDefault).
func (c *rewritingComparator) exprCount(e expressions.RelationalExpression, filter func(expressions.RelationalExpression) bool, visiting map[*expressions.Reference]bool) int {
	if e == nil {
		return 0
	}
	count := 0
	if filter == nil || filter(e) {
		count = 1
	}
	for _, q := range e.GetQuantifiers() {
		c.descend(q.GetRangesOver(), visiting, func(child expressions.RelationalExpression) {
			count += c.exprCount(child, filter, visiting)
		})
	}
	return count
}

// residualConjuncts counts the CNF full-size of every predicate on the tree
// (Java NormalizedResidualPredicateProperty: the node's own predicates plus
// the recursion through each child's single final). It counts LOGICAL
// predicate carriers (RelationalExpressionWithPredicates) and the physical
// filter/NLJ nodes alike, so the tier is live on the all-logical REWRITING
// memo.
//
// The count comes from the NORMALIZER's size function, because that is what
// Java's property returns: countNormalizedConjuncts
// (NormalizedResidualPredicateProperty.java:81-90) is
// `getDefaultInstanceForCnf().getMetrics(p).getNormalFormFullSize()`, which
// sizes `NOT(a OR b)` as 2, a negated Or being a major whose children SUM.
func (c *rewritingComparator) residualConjuncts(e expressions.RelationalExpression, visiting map[*expressions.Reference]bool) int {
	if e == nil {
		return 0
	}
	count := 0
	if wp, ok := e.(expressions.RelationalExpressionWithPredicates); ok {
		for _, p := range wp.GetPredicates() {
			count += int(normalFormSize(p, false, normalFormCNF))
		}
	}
	for _, q := range e.GetQuantifiers() {
		c.descend(q.GetRangesOver(), visiting, func(child expressions.RelationalExpression) {
			count += c.residualConjuncts(child, visiting)
		})
	}
	return count
}

// predCountByLevel fills counts[level] with predicate counts by tree depth
// (level 0 = leaves; Java PredicateCountByLevelProperty). Returns the node's
// level.
func (c *rewritingComparator) predCountByLevel(e expressions.RelationalExpression, counts map[int]int, visiting map[*expressions.Reference]bool) int {
	if e == nil {
		return -1
	}
	maxChildLevel := -1
	for _, q := range e.GetQuantifiers() {
		c.descend(q.GetRangesOver(), visiting, func(child expressions.RelationalExpression) {
			if lvl := c.predCountByLevel(child, counts, visiting); lvl > maxChildLevel {
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

// deepHash hashes the tree: schema-neutral tie-break node content folded with
// each child edge's quantifier attributes and the children's hashes,
// order-sensitive (see deepHashCode's commutative-XOR caution). Memo identity
// remains independently schema-aware.
//
// The EDGE ATTRIBUTES (kind / null-on-empty / strict-single) must be folded:
// they live on the quantifier, so HashCodeWithoutChildren cannot see them and
// the child content is identical — two finals differing only in a quantifier's
// flag (a LEFT box vs its INNER twin) would otherwise tie through every tier
// and leave the winner insertion-order dependent. Folded regardless of whether
// the descent leafs at a back-edge — the edge itself is part of the tree's
// identity.
func (c *rewritingComparator) deepHash(e expressions.RelationalExpression, visiting map[*expressions.Reference]bool) uint64 {
	if e == nil {
		return 0
	}
	h := tieBreakNodeHash(e)
	for _, q := range e.GetQuantifiers() {
		// attrs == 0 is the default edge (plain ForEach): folded as a no-op so
		// attribute-default trees keep their exact hash — only a variant edge
		// perturbs. The guarantee needed is variant ≠ plain, which a one-sided
		// fold provides.
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
		c.descend(q.GetRangesOver(), visiting, func(child expressions.RelationalExpression) {
			childHash := c.deepHash(child, visiting)
			h = h*0x100000001b3 ^ (childHash*0x517cc1b727220a95 + 0x6c62272e07bb0142)
		})
	}
	return h
}

// isLeftOuterJoinSelect is Go's OuterJoinExpression: FULL OUTER has no
// canonical form to prefer, so only LEFT is counted.
func isLeftOuterJoinSelect(e expressions.RelationalExpression) bool {
	sel, ok := e.(*expressions.SelectExpression)
	return ok && sel.GetJoinType() == expressions.JoinLeftOuter
}

// RewritingPruneError is Java's Verify(finalMembers.size() == 1) in the
// REWRITING cost properties (ExpressionCountProperty.forReference): a
// REWRITING alternative was costed over a child group REWRITING had not pruned
// to one final. A planner defect, never a property of the query.
type RewritingPruneError struct {
	Finals int
}

func (e *RewritingPruneError) Error() string {
	return fmt.Sprintf("planner: a REWRITING alternative was costed over a child group with %d final members, want exactly 1", e.Finals)
}
