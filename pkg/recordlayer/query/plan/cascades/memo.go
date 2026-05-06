package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
)

// Memo is the central memoization structure for the Cascades planner.
// It tracks all References in the plan DAG and enables cross-Reference
// equivalence-class sharing: when two rules independently derive
// structurally-equivalent sub-expressions, the Memo routes them into
// the same Reference so the planner explores/optimizes that sub-tree
// only once.
//
// Ports the memoization behaviour from Java's CascadesRuleCall +
// Traversal. Java uses a MutableNetwork<Reference, ReferencePath> to
// track the DAG topology; Go uses flat index maps for the same purpose.
//
// Lifecycle:
//   - Created via NewMemo(rootRef) at planner construction time.
//   - Rules call MemoizeExpression(expr) to find-or-create a Reference
//     for a sub-expression. Returns an existing Reference when the
//     expr (or a structural equivalent) is already memoized.
//   - The Planner's EXPLORE phase populates the Memo as rules fire.
//
// Thread safety: single-threaded (same as Java's planner).
type Memo struct {
	root *expressions.Reference

	// refs is the set of all References known to the Memo.
	refs map[*expressions.Reference]struct{}

	// childToParents maps a child Reference to parent References that
	// have a member expression with a Quantifier ranging over it.
	// Used for topological lookup during memoization.
	childToParents map[*expressions.Reference][]parentEdge

	// leafRefs tracks References holding leaf expressions (no
	// quantifiers). Stored as a SLICE for deterministic iteration
	// order — Go map iteration is randomized, and non-deterministic
	// lookup order causes the Planner to be non-deterministic when
	// the self-reference guard creates duplicate leaf References
	// that later get indexed.
	leafRefs []*expressions.Reference

	// leafRefsSet mirrors leafRefs for O(1) containment checks.
	leafRefsSet map[*expressions.Reference]struct{}
}

// parentEdge records that `parent` has a member `expr` with a
// Quantifier ranging over some child Reference (the map key in
// childToParents).
type parentEdge struct {
	parent *expressions.Reference
	expr   expressions.RelationalExpression
}

// NewMemo constructs a Memo rooted at `root` and indexes the full DAG
// reachable from it. If root is nil, returns an empty Memo.
func NewMemo(root *expressions.Reference) *Memo {
	m := &Memo{
		root:           root,
		refs:           make(map[*expressions.Reference]struct{}),
		childToParents: make(map[*expressions.Reference][]parentEdge),
		leafRefsSet:    make(map[*expressions.Reference]struct{}),
	}
	if root != nil {
		m.indexReference(root)
	}
	return m
}

// Root returns the root Reference of the Memo.
func (m *Memo) Root() *expressions.Reference {
	return m.root
}

// References returns all References known to the Memo. The returned map
// is read-only; callers must not mutate it.
func (m *Memo) References() map[*expressions.Reference]struct{} {
	return m.refs
}

// ContainsReference reports whether the Memo has indexed `ref`.
func (m *Memo) ContainsReference(ref *expressions.Reference) bool {
	_, ok := m.refs[ref]
	return ok
}

// RegisterReference adds a Reference (and its sub-tree) to the Memo's
// index without performing memoization lookup. Used when a rule creates
// a Reference that is known-fresh (e.g. the root at construction time,
// or a Reference already checked by MemoizeExpression).
func (m *Memo) RegisterReference(ref *expressions.Reference) {
	m.indexReference(ref)
}

// MemoizeExpression is the core memoization entry point. Given an
// expression, it either:
//   - finds an existing Reference in the Memo that already contains a
//     structurally-equivalent expression (same node info under
//     EqualsWithoutChildren + same child References by pointer), and
//     returns that Reference; or
//   - creates a new single-member Reference for the expression,
//     registers it in the Memo, and returns the new Reference.
//
// This is how cross-Reference sharing works: two rules that
// independently produce the same sub-expression get back the SAME
// Reference, avoiding redundant exploration.
//
// Mirrors Java's CascadesRuleCall.memoizeExploratoryExpressions.
func (m *Memo) MemoizeExpression(expr expressions.RelationalExpression) *expressions.Reference {
	if expr == nil {
		panic("Memo.MemoizeExpression: nil expression")
	}

	qs := expr.GetQuantifiers()

	// Leaf path: expression has no children.
	if len(qs) == 0 {
		return m.memoizeLeaf(expr)
	}

	// Non-leaf path: use child References as lookup keys.
	return m.memoizeNonLeaf(expr, qs)
}

// MemoizeExpressions memoizes multiple expressions into a single
// Reference. If an existing Reference contains ALL of the given
// expressions (or structural equivalents), returns that Reference.
// Otherwise creates a new Reference holding all expressions.
//
// Used when a rule produces multiple equivalent alternatives for
// the same sub-tree.
func (m *Memo) MemoizeExpressions(exprs []expressions.RelationalExpression) *expressions.Reference {
	if len(exprs) == 0 {
		panic("Memo.MemoizeExpressions: empty expression list")
	}
	if len(exprs) == 1 {
		return m.MemoizeExpression(exprs[0])
	}

	// Try to find an existing Reference that contains all expressions.
	// Use the first expression's topology for the lookup.
	first := exprs[0]
	qs := first.GetQuantifiers()

	if len(qs) == 0 {
		// All must be leaves for leaf-path.
		for _, ref := range m.leafRefsSlice() {
			if m.refContainsAll(ref, exprs) {
				return ref
			}
		}
	} else {
		candidates := m.findCandidateParents(qs)
		for _, ref := range candidates {
			if m.refContainsAll(ref, exprs) {
				return ref
			}
		}
	}

	// Not found — create a new Reference holding all expressions.
	ref := expressions.InitialOf(first)
	for _, e := range exprs[1:] {
		ref.Insert(e)
	}
	m.indexReference(ref)
	return ref
}

// AddExpression registers a new expression into the Memo's index
// within an existing Reference. Call this after Reference.Insert
// succeeds to keep the Memo's topology index up to date.
func (m *Memo) AddExpression(ref *expressions.Reference, expr expressions.RelationalExpression) {
	for _, q := range expr.GetQuantifiers() {
		child := q.GetRangesOver()
		if child == nil {
			continue
		}
		m.childToParents[child] = append(m.childToParents[child], parentEdge{
			parent: ref,
			expr:   expr,
		})
		// Ensure child is indexed.
		if _, known := m.refs[child]; !known {
			m.indexReference(child)
		}
	}
	if len(expr.GetQuantifiers()) == 0 {
		m.addLeafRef(ref)
	}
}

// memoizeLeaf handles the leaf case: no children.
func (m *Memo) memoizeLeaf(expr expressions.RelationalExpression) *expressions.Reference {
	h := expr.HashCodeWithoutChildren()
	for _, ref := range m.leafRefs {
		for _, member := range ref.Members() {
			if member.HashCodeWithoutChildren() == h &&
				member.EqualsWithoutChildren(expr, expressions.EmptyAliasMap()) {
				return ref
			}
		}
	}
	// Not found — create fresh.
	ref := expressions.InitialOf(expr)
	m.refs[ref] = struct{}{}
	m.addLeafRef(ref)
	return ref
}

// memoizeNonLeaf handles the non-leaf case: use child References for
// topological lookup.
func (m *Memo) memoizeNonLeaf(expr expressions.RelationalExpression, qs []expressions.Quantifier) *expressions.Reference {
	candidates := m.findCandidateParents(qs)

	// Check each candidate for structural containment.
	h := expr.HashCodeWithoutChildren()
	for _, ref := range candidates {
		for _, member := range ref.Members() {
			if member.HashCodeWithoutChildren() != h {
				continue
			}
			if !member.EqualsWithoutChildren(expr, expressions.EmptyAliasMap()) {
				continue
			}
			// Node-info matches. Check child References match by pointer.
			if sameChildRefs(member, expr) {
				return ref
			}
		}
	}

	// Not found — create fresh and index it.
	ref := expressions.InitialOf(expr)
	m.refs[ref] = struct{}{}
	for _, q := range qs {
		child := q.GetRangesOver()
		if child == nil {
			continue
		}
		m.childToParents[child] = append(m.childToParents[child], parentEdge{
			parent: ref,
			expr:   expr,
		})
	}
	return ref
}

// findCandidateParents returns References that are parents of ALL of
// the given Quantifiers' child References. This is the topological
// intersection that narrows down candidates for memoization.
// Results are returned in insertion order (deterministic).
func (m *Memo) findCandidateParents(qs []expressions.Quantifier) []*expressions.Reference {
	if len(qs) == 0 {
		return nil
	}

	// Start with parents of the first child Reference (in edge order).
	first := qs[0].GetRangesOver()
	if first == nil {
		return nil
	}
	edges := m.childToParents[first]
	if len(edges) == 0 {
		return nil
	}

	// Collect parent References from the first child in edge order
	// (insertion order, deterministic).
	var candidateOrder []*expressions.Reference
	candidates := make(map[*expressions.Reference]struct{}, len(edges))
	for _, e := range edges {
		if _, seen := candidates[e.parent]; !seen {
			candidates[e.parent] = struct{}{}
			candidateOrder = append(candidateOrder, e.parent)
		}
	}

	// Intersect with parents of subsequent child References.
	for _, q := range qs[1:] {
		child := q.GetRangesOver()
		if child == nil {
			return nil
		}
		childEdges := m.childToParents[child]
		if len(childEdges) == 0 {
			return nil
		}
		childParents := make(map[*expressions.Reference]struct{}, len(childEdges))
		for _, e := range childEdges {
			childParents[e.parent] = struct{}{}
		}
		// Intersect: filter candidateOrder in-place.
		n := 0
		for _, c := range candidateOrder {
			if _, ok := childParents[c]; ok {
				candidateOrder[n] = c
				n++
			} else {
				delete(candidates, c)
			}
		}
		candidateOrder = candidateOrder[:n]
		if n == 0 {
			return nil
		}
	}

	return candidateOrder
}

// refContainsAll checks whether ref contains a structural equivalent
// for every expression in exprs.
func (m *Memo) refContainsAll(ref *expressions.Reference, exprs []expressions.RelationalExpression) bool {
	for _, expr := range exprs {
		if !m.refContains(ref, expr) {
			return false
		}
	}
	return true
}

// refContains checks whether ref contains a structural equivalent of
// expr (same node info + same child References by pointer).
func (m *Memo) refContains(ref *expressions.Reference, expr expressions.RelationalExpression) bool {
	h := expr.HashCodeWithoutChildren()
	for _, member := range ref.Members() {
		if member.HashCodeWithoutChildren() != h {
			continue
		}
		if !member.EqualsWithoutChildren(expr, expressions.EmptyAliasMap()) {
			continue
		}
		if sameChildRefs(member, expr) {
			return true
		}
	}
	return false
}

// sameChildRefs returns true if a and b have the same Quantifier count
// and every Quantifier's Reference is the same pointer.
func sameChildRefs(a, b expressions.RelationalExpression) bool {
	aQs := a.GetQuantifiers()
	bQs := b.GetQuantifiers()
	if len(aQs) != len(bQs) {
		return false
	}
	for i := range aQs {
		if aQs[i].GetRangesOver() != bQs[i].GetRangesOver() {
			return false
		}
	}
	return true
}

// indexReference recursively indexes ref and all References reachable
// through it. Skips already-indexed References.
func (m *Memo) indexReference(ref *expressions.Reference) {
	if ref == nil {
		return
	}
	if _, known := m.refs[ref]; known {
		return
	}
	m.refs[ref] = struct{}{}

	members := ref.Members()
	isLeaf := true
	for _, member := range members {
		qs := member.GetQuantifiers()
		if len(qs) > 0 {
			isLeaf = false
		}
		for _, q := range qs {
			child := q.GetRangesOver()
			if child == nil {
				continue
			}
			m.childToParents[child] = append(m.childToParents[child], parentEdge{
				parent: ref,
				expr:   member,
			})
			m.indexReference(child)
		}
	}
	if isLeaf {
		m.addLeafRef(ref)
	}
}

// addLeafRef appends ref to the leafRefs slice (if not already present).
func (m *Memo) addLeafRef(ref *expressions.Reference) {
	if _, ok := m.leafRefsSet[ref]; ok {
		return
	}
	m.leafRefsSet[ref] = struct{}{}
	m.leafRefs = append(m.leafRefs, ref)
}

// leafRefsSlice returns the leaf References as a slice (for iteration).
func (m *Memo) leafRefsSlice() []*expressions.Reference {
	return m.leafRefs
}
