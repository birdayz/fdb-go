package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
)

// Traversal is a pre-computed walk of an expression DAG rooted at a
// Reference. It indexes expressions by their References for efficient
// lookup during the matching phase.
//
// Ports the core surface of Java's
// `com.apple.foundationdb.record.query.plan.cascades.Traversal`.
// Java uses a Guava MutableNetwork<Reference, ReferencePath>; the Go
// version uses maps and slices for the same semantics without the
// external dependency. The traversal is immutable after construction —
// callers must not mutate the underlying expression DAG.
type Traversal struct {
	rootRef *expressions.Reference

	// All (ref, expr) pairs found during the walk, in DFS order.
	refExprPairs []refExprPair

	// Index: Reference -> list of expressions that are members of that Reference.
	refToExprs map[*expressions.Reference][]expressions.RelationalExpression

	// Index: child Reference -> list of (parentRef, parentExpr) that
	// reference it via a quantifier. Mirrors Java's network outEdges:
	// given a child reference, find the parent (ref, expr) pairs that
	// own a quantifier ranging over it.
	childToParents map[*expressions.Reference][]refExprPair

	// Leaf references: references containing at least one leaf
	// expression (no quantifiers). Mirrors Java's leafReferences set.
	leafRefs []*expressions.Reference
}

// refExprPair holds a (Reference, RelationalExpression) pair — the
// expression is a member of the Reference.
type refExprPair struct {
	ref  *expressions.Reference
	expr expressions.RelationalExpression
}

// NewTraversal builds a Traversal by walking the expression DAG from
// rootRef using a recursive DFS, visiting each Reference at most once.
//
// Mirrors Java's `Traversal.withRoot` + `collectNetwork`.
func NewTraversal(rootRef *expressions.Reference) *Traversal {
	t := &Traversal{
		rootRef:        rootRef,
		refToExprs:     make(map[*expressions.Reference][]expressions.RelationalExpression),
		childToParents: make(map[*expressions.Reference][]refExprPair),
	}

	visited := make(map[*expressions.Reference]bool)
	t.collectNetwork(visited, rootRef)

	return t
}

// collectNetwork recursively walks the expression DAG starting from
// ref, recording all (ref, expr) pairs and building the parent index.
// Each Reference is visited at most once (tracked by visited map).
// Mirrors Java's Traversal.collectNetwork.
func (t *Traversal) collectNetwork(visited map[*expressions.Reference]bool, ref *expressions.Reference) {
	if visited[ref] {
		return
	}
	visited[ref] = true

	anyLeaf := false
	for _, expr := range ref.AllMembers() {
		t.refExprPairs = append(t.refExprPairs, refExprPair{ref: ref, expr: expr})
		t.refToExprs[ref] = append(t.refToExprs[ref], expr)

		quantifiers := expr.GetQuantifiers()
		if len(quantifiers) == 0 {
			anyLeaf = true
		} else {
			// Descend into each quantifier's child reference.
			for _, q := range quantifiers {
				childRef := q.GetRangesOver()
				t.collectNetwork(visited, childRef)
				// Record that (ref, expr) is a parent of childRef.
				t.childToParents[childRef] = append(
					t.childToParents[childRef],
					refExprPair{ref: ref, expr: expr},
				)
			}
		}
	}

	if anyLeaf {
		t.leafRefs = append(t.leafRefs, ref)
	}
}

// GetRootReference returns the root reference of the traversal.
func (t *Traversal) GetRootReference() *expressions.Reference {
	return t.rootRef
}

// GetLeafReferences returns references that contain at least one leaf
// expression (an expression with zero quantifiers). Used by
// MatchLeafRule to find the bottom of the candidate's expression tree.
func (t *Traversal) GetLeafReferences() []*expressions.Reference {
	return t.leafRefs
}

// GetParentRefPairs returns all (parentRef, parentExpr) pairs that
// reference the given child Reference via a quantifier. Mirrors Java's
// Traversal.getParentRefPaths (outEdges in the network).
func (t *Traversal) GetParentRefPairs(childRef *expressions.Reference) []refExprPair {
	return t.childToParents[childRef]
}

// FindReferencingExpressions returns a map from Reference to the set
// of expressions that reference any of the given child References
// through their quantifiers. Used by MatchIntermediateRule to walk
// upward from already-matched leaves.
//
// For each childRef in the input, looks up its parents in the
// childToParents index and collects all parent (ref, expr) pairs.
// Deduplicates: if the same (ref, expr) pair appears for multiple
// childRefs, only includes it once.
func (t *Traversal) FindReferencingExpressions(
	childRefs []*expressions.Reference,
) map[*expressions.Reference][]expressions.RelationalExpression {
	result := make(map[*expressions.Reference][]expressions.RelationalExpression)

	// Track seen (ref, expr) pairs by identity to avoid duplicates.
	type pairKey struct {
		ref  *expressions.Reference
		expr expressions.RelationalExpression
	}
	seen := make(map[pairKey]bool)

	for _, childRef := range childRefs {
		for _, parent := range t.childToParents[childRef] {
			key := pairKey{ref: parent.ref, expr: parent.expr}
			if seen[key] {
				continue
			}
			seen[key] = true
			result[parent.ref] = append(result[parent.ref], parent.expr)
		}
	}

	return result
}
