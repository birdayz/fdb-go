package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
)

// Cross-Reference equivalence-class merging (RFC-037, the Cascades-paper
// "merge two groups discovered to be one", §2 + §3.5). This is a Go-only
// extension beyond Java (which, like the pre-RFC Go memo, only interns at
// insertion time and never merges two distinct groups discovered later to
// be equivalent). Wire compatibility is untouched: this only changes how
// the planner shares/optimizes logically-equivalent sub-expressions.
//
// Scope: REWRITING phase only. PLANNING-phase bookkeeping (winners,
// partial matches) holds/embeds References that the merge does not
// canonicalize, so merge panics if asked to touch a Reference carrying
// them — a deliberate tripwire (see Reference.HasWinnersOrMatches).

// Integrate is the cross-group merge entry point, called when a rule
// yields an expression into ref during REWRITING. It either:
//   - discovers that a structurally-equivalent member already lives in a
//     DIFFERENT Reference and merges the two groups, or
//   - records ref as a parent of expr's child References in the topology
//     index (so future lookups can find this parent).
//
// After the initial step it drains the recursive bottom-up worklist: a
// merge can make the merged group's parents duplicates of one another,
// which must themselves merge (the paper's recursive integration).
func (m *Memo) Integrate(ref *expressions.Reference, expr expressions.RelationalExpression) {
	if m == nil || ref == nil || expr == nil {
		return
	}
	m.integrateOne(ref.Canonical(), expr)
	for len(m.pendingReintegrate) > 0 {
		w := m.pendingReintegrate[0]
		m.pendingReintegrate = m.pendingReintegrate[1:]
		w = w.Canonical()
		// Snapshot: integrateOne may merge and mutate childToParents.
		edges := append([]parentEdge(nil), m.childToParents[w]...)
		for _, e := range edges {
			m.integrateOne(e.parent.Canonical(), e.expr)
		}
	}
}

// integrateOne performs one step of integration without draining the
// worklist (merge appends to it; Integrate drains).
//
// A merge is performed only when NEITHER group carries PLANNING-phase
// bookkeeping (winners / partial matches). Those structures embed raw
// References the merge does not canonicalize, so merging them would be
// unsound. The Explore() entry point interleaves optimization with
// exploration, so a Reference can already hold a winner when a later
// yield finds an equivalent group; in that case we leave the two groups
// separate (the pre-RFC behaviour) rather than merge unsoundly.
func (m *Memo) integrateOne(ref *expressions.Reference, expr expressions.RelationalExpression) {
	ref = ref.Canonical()
	if other := m.findEquivalentRef(expr, ref); other != nil {
		if m.mergeable(ref, other) {
			m.merge(ref, other)
			return
		}
	}
	m.indexExpr(ref, expr)
}

// mergeable reports whether two equivalent groups may be merged. It
// forbids:
//   - merging a group that carries PLANNING-phase bookkeeping (winners /
//     partial matches embed raw References the merge can't canonicalize); and
//   - merging a group with one of its own ancestors/descendants, which
//     would make a member range over the merged group itself and create a
//     cycle in the expression DAG. This happens for idempotence rewrites
//     (e.g. DistinctMergeRule yields Distinct(→D) into a group whose child
//     already IS Distinct(→D)). Such equivalences are already captured by
//     the simplification rule within a single group; merging the groups
//     adds nothing but a cycle. The redundant-subexpression / shared-sub-
//     product value lives in SIBLING equivalences, which are unaffected.
func (m *Memo) mergeable(a, b *expressions.Reference) bool {
	a, b = a.Canonical(), b.Canonical()
	if a == b {
		return false
	}
	if a.HasWinnersOrMatches() || b.HasWinnersOrMatches() {
		return false
	}
	return !m.reachable(a, b) && !m.reachable(b, a)
}

// reachable reports whether target is reachable downward from `from`
// through the memo DAG (following canonical child References). Used to
// detect ancestor/descendant relationships before a merge.
func (m *Memo) reachable(from, target *expressions.Reference) bool {
	from, target = from.Canonical(), target.Canonical()
	visited := map[*expressions.Reference]struct{}{}
	var walk func(r *expressions.Reference) bool
	walk = func(r *expressions.Reference) bool {
		r = r.Canonical()
		if r == target {
			return true
		}
		if _, seen := visited[r]; seen {
			return false
		}
		visited[r] = struct{}{}
		for _, mem := range r.Members() {
			for _, q := range mem.GetQuantifiers() {
				if c := q.GetRangesOver(); c != nil && walk(c) {
					return true
				}
			}
		}
		return false
	}
	for _, mem := range from.Members() {
		for _, q := range mem.GetQuantifiers() {
			if c := q.GetRangesOver(); c != nil && walk(c) {
				return true
			}
		}
	}
	return false
}

// findEquivalentRef returns a Reference (other than exclude) that holds a
// member structurally equivalent to expr — same node info under
// EqualsWithoutChildren and the same canonical child References. Returns
// nil if no such Reference exists. Uses the same topological narrowing as
// MemoizeExpression so the scan is bounded.
func (m *Memo) findEquivalentRef(expr expressions.RelationalExpression, exclude *expressions.Reference) *expressions.Reference {
	exclude = exclude.Canonical()
	h := expr.HashCodeWithoutChildren()
	qs := expr.GetQuantifiers()

	if len(qs) == 0 {
		for _, ref := range m.leafRefs {
			ref = ref.Canonical()
			if ref == exclude {
				continue
			}
			for _, member := range ref.Members() {
				// Alias-aware (RFC-039 PR-A activation). Leaves have no
				// quantifiers, so MemoEqual reduces to node-info equality;
				// hash gate (alias-invariant) first.
				if member.HashCodeWithoutChildren() == h && expressions.MemoEqual(member, expr) {
					return ref
				}
			}
		}
		return nil
	}

	for _, cand := range m.findCandidateParents(qs) {
		cand = cand.Canonical()
		if cand == exclude {
			continue
		}
		for _, member := range cand.Members() {
			if member.HashCodeWithoutChildren() != h {
				continue
			}
			// Alias-aware merge-candidate match: equivalent-up-to-quantifier-
			// alias members now merge (replaces EqualsWithoutChildren(empty)
			// + pointer-identical sameChildRefs).
			if expressions.MemoEqual(member, expr) {
				return cand
			}
		}
	}
	return nil
}

// indexExpr records ref as a parent of every child Reference of expr in
// the topology index (keyed by canonical child), deduplicating edges, and
// registers leaf expressions in the leaf index. Idempotent.
func (m *Memo) indexExpr(ref *expressions.Reference, expr expressions.RelationalExpression) {
	ref = ref.Canonical()
	qs := expr.GetQuantifiers()
	if len(qs) == 0 {
		m.addLeafRef(ref)
		return
	}
	for _, q := range qs {
		child := q.GetRangesOver()
		if child == nil {
			continue
		}
		if _, known := m.refs[child]; !known {
			m.indexReference(child)
		}
		m.addParentEdge(child, ref, expr)
	}
}

// addParentEdge appends a (parent, expr) edge under child in
// childToParents unless an identical edge is already present.
func (m *Memo) addParentEdge(child, parent *expressions.Reference, expr expressions.RelationalExpression) {
	edges := m.childToParents[child]
	for _, e := range edges {
		if e.parent == parent && e.expr == expr {
			return
		}
	}
	m.childToParents[child] = append(edges, parentEdge{parent: parent, expr: expr})
}

// merge collapses two equivalent groups into one (RFC-037 §3). The
// survivor is the Reference with the lower id (deterministic, independent
// of map iteration order). It folds the loser's members and exploration
// state into the survivor, marks the loser as forwarding, repoints the
// Memo's topology index, invalidates correlation caches up the DAG, and
// queues the survivor for recursive parent re-integration.
func (m *Memo) merge(a, b *expressions.Reference) {
	a, b = a.Canonical(), b.Canonical()
	if a == b {
		return
	}
	// Scope tripwire: cross-group merging is REWRITING-only. Partial
	// matches / winners are PLANNING artifacts that embed un-canonicalized
	// References; merging in their presence would be unsound.
	if a.HasWinnersOrMatches() || b.HasWinnersOrMatches() {
		panic("cascades: cross-group merge on a Reference carrying PLANNING-phase winners/partial matches (RFC-037 is REWRITING-only)")
	}

	m.track(a)
	m.track(b)
	winner, loser := a, b
	if loser.ID() < winner.ID() {
		winner, loser = loser, winner
	}

	winner.Absorb(loser) // folds members + exploration state; sets loser.forwardedTo=winner
	m.repointIndices(loser, winner)
	m.invalidateCorrelatedUp(winner)
	m.mergeCount++
	m.pendingReintegrate = append(m.pendingReintegrate, winner)
}

// repointIndices rewrites the Memo's topology index after loser has been
// folded into winner: edges where loser is the child key move to winner's
// key; edges where loser is the parent become winner; loser is removed
// from refs/leafRefs; the root advances if it was the loser.
func (m *Memo) repointIndices(loser, winner *expressions.Reference) {
	// 1. loser as a child (map key): its parents now range over winner.
	if edges, ok := m.childToParents[loser]; ok {
		for _, e := range edges {
			m.addParentEdge(winner, e.parent, e.expr)
		}
		delete(m.childToParents, loser)
	}
	// 2. loser as a parent (edge value): redirect to winner, dedup.
	for child, edges := range m.childToParents {
		changed := false
		for i := range edges {
			if edges[i].parent == loser {
				edges[i].parent = winner
				changed = true
			}
		}
		if changed {
			m.childToParents[child] = dedupEdges(edges)
		}
	}
	// 3. ref/leaf sets and root. winner is canonical here (merge
	// canonicalizes both sides before calling repointIndices), so no
	// guard is needed before re-checking its leaf status.
	delete(m.refs, loser)
	m.removeLeafRef(loser)
	m.addLeafRefIfLeaf(winner)
	if m.root == loser {
		m.root = winner
	}
}

// addLeafRefIfLeaf adds winner to the leaf index if all its members are
// leaves (no quantifiers). After absorbing a leaf loser, winner may now
// hold leaf members.
func (m *Memo) addLeafRefIfLeaf(winner *expressions.Reference) {
	for _, member := range winner.Members() {
		if len(member.GetQuantifiers()) > 0 {
			return
		}
	}
	if len(winner.Members()) > 0 {
		m.addLeafRef(winner)
	}
}

// dedupEdges removes duplicate (parent, expr) edges, preserving order.
func dedupEdges(edges []parentEdge) []parentEdge {
	out := edges[:0]
	for _, e := range edges {
		dup := false
		for _, k := range out {
			if k.parent == e.parent && k.expr == e.expr {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, e)
		}
	}
	return out
}

// invalidateCorrelatedUp drops the correlated-to cache on winner and every
// ancestor reachable upward through childToParents. Equivalent groups have
// equal correlation sets, so the survivor's value is unchanged; this is
// defensive insurance against any ancestor cache built from the loser's
// pre-merge member set (RFC-037 §3 step 5).
func (m *Memo) invalidateCorrelatedUp(ref *expressions.Reference) {
	visited := map[*expressions.Reference]struct{}{}
	queue := []*expressions.Reference{ref.Canonical()}
	for len(queue) > 0 {
		r := queue[0].Canonical()
		queue = queue[1:]
		if _, seen := visited[r]; seen {
			continue
		}
		visited[r] = struct{}{}
		r.InvalidateCorrelatedToCache()
		for _, e := range m.childToParents[r] {
			queue = append(queue, e.parent.Canonical())
		}
	}
}

// removeLeafRef drops ref from the leaf index (slice + set).
func (m *Memo) removeLeafRef(ref *expressions.Reference) {
	if _, ok := m.leafRefsSet[ref]; !ok {
		return
	}
	delete(m.leafRefsSet, ref)
	for i, r := range m.leafRefs {
		if r == ref {
			m.leafRefs = append(m.leafRefs[:i], m.leafRefs[i+1:]...)
			break
		}
	}
}
