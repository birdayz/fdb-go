package cascades

import (
	"strconv"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
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

	// nextID hands out monotonic identities to References on first
	// registration. Merge picks the lower id as the survivor, making
	// the winner a deterministic function of registration order (which
	// follows the deterministic task schedule) rather than map order.
	// Starts at 1 so an unregistered Reference (id 0) is distinguishable.
	nextID uint64

	// mergeCount counts cross-group merges performed (RFC-037). Exposed
	// via MergeCount for tests that assert the optimization fires.
	mergeCount int

	// mergeAliasCounter hands out per-plan deterministic merge-quantifier
	// aliases for PartitionSelectRule's N-way join re-enumeration (RFC-077
	// 7.5). It is per-Memo (one Memo per Plan call), so the SAME query planned
	// twice mints the SAME alias sequence in the SAME deterministic exploration
	// order → a STABLE plan hash across plannings (the process-global
	// UniqueCorrelationIdentifier counter would leak its absolute value into the
	// NLJ source alias and the plan hash, churning plan-log identity + the
	// cost-model tiebreak across a process's history). Distinct merge occurrences
	// within one plan still get DISTINCT aliases, so equivalent sub-products are
	// interned by the alias-aware Reference.Insert tier (not by a stable string).
	mergeAliasCounter uint64

	// pendingReintegrate is the worklist for the paper's recursive
	// bottom-up merge: after merging two groups, their parents may have
	// become duplicates. Drained at the top of Integrate.
	pendingReintegrate []*expressions.Reference
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
		nextID:         1,
	}
	if root != nil {
		m.indexReference(root)
	}
	return m
}

// track registers ref in the Memo's ref set and assigns it a monotonic
// id on first registration (idempotent).
func (m *Memo) track(ref *expressions.Reference) {
	m.refs[ref] = struct{}{}
	if ref.ID() == 0 {
		ref.AssignMemoID(m.nextID)
		m.nextID++
	}
}

// MergeCount returns the number of cross-group merges performed so far
// (RFC-037). Used by tests to assert the merge optimization fires.
func (m *Memo) MergeCount() int { return m.mergeCount }

// NextMergeAlias returns a per-plan deterministic, collision-PROOF quantifier
// alias for a PartitionSelectRule merge sub-join (RFC-077 7.5).
//
// The alias embeds a double-quote ("). That is the one character no parsed SQL
// identifier can ever contain: the lexer's delimited-identifier rule is
// DOUBLE_QUOTE_ID: '"' ~'"'+ '"' (RelationalLexer.g4) — a quoted identifier is
// any run of NON-quote characters between quotes, so the quotes are stripped and
// the resulting name can never include a ". A bare "$m"-prefix is NOT safe on its
// own: a user could write a quoted alias `AS "$m1"`, which parses to the name
// "$m1" and would collide with this merge quantifier, corrupting alias-keyed
// binding/rebasing in a multi-way join (codex P2). (This collision class also
// affects UniqueCorrelationIdentifier's "q$N" — `AS "q$1"` — a pre-existing,
// separate hardening item; here we make the merge alias uncollidable outright.)
//
// The per-Memo ordinal makes the alias deterministic across plannings of the same
// query (for a stable plan hash) while still differing per merge occurrence, so
// equivalent sub-products intern via the alias-aware Reference.Insert tier, not via
// a stable string. The alias is internal — never re-lexed as SQL; it only appears
// as a correlation key (rebasing, NLJ source alias, Explain). See mergeAliasCounter.
func (m *Memo) NextMergeAlias() values.CorrelationIdentifier {
	m.mergeAliasCounter++
	return values.NamedCorrelationIdentifier(`$m"` + strconv.FormatUint(m.mergeAliasCounter, 10))
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
		// Ensure child is indexed.
		if _, known := m.refs[child]; !known {
			m.indexReference(child)
		}
		// Dedup edges (via addParentEdge) so repeated indexing of the
		// same (parent, expr) — e.g. SaturationCheckTask after a yield
		// already integrated by RFC-037's Integrate hook — does not grow
		// duplicate parent edges.
		m.addParentEdge(child, ref, expr)
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
	m.track(ref)
	m.addLeafRef(ref)
	return ref
}

// memoizeNonLeaf handles the non-leaf case: use child References for
// topological lookup.
func (m *Memo) memoizeNonLeaf(expr expressions.RelationalExpression, qs []expressions.Quantifier) *expressions.Reference {
	candidates := m.findCandidateParents(qs)

	// Check each candidate for alias-aware containment (RFC-039 PR-A
	// activation): MemoEqual builds the node's own quantifier-alias map, so
	// members equivalent up to a consistent quantifier-alias renaming intern
	// into the SAME Reference (the prior alias-sensitive interning gave them
	// distinct References). Children intern alias-aware bottom-up, so the
	// topological candidate narrowing surfaces equivalent parents.
	h := expr.HashCodeWithoutChildren()
	for _, ref := range candidates {
		for _, member := range ref.Members() {
			if member.HashCodeWithoutChildren() != h {
				continue
			}
			if expressions.MemoEqual(member, expr) {
				return ref
			}
		}
	}

	// Not found — create fresh and index it.
	ref := expressions.InitialOf(expr)
	m.track(ref)
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
		// Alias-aware (RFC-039 PR-A activation).
		if expressions.MemoEqual(member, expr) {
			return true
		}
	}
	return false
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
	m.track(ref)

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
