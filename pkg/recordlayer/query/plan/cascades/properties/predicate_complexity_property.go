package properties

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
)

// EvaluatePredicateComplexity walks the expression tree and returns
// the maximum "diameter" (widest level in the tree) of any predicate
// attached to a RelationalExpressionWithPredicates node. Matches
// Java's PredicateComplexityProperty.evaluate, which computes
// TreeLike.diameterWithLevelCounting for each predicate and returns
// the max across the entire expression tree.
//
// A higher value means more complex predicate structure. Zero means
// no predicates exist in the tree.
func EvaluatePredicateComplexity(expr expressions.RelationalExpression) int {
	if expr == nil {
		return 0
	}
	nodeMax := 0
	if wp, ok := expr.(expressions.RelationalExpressionWithPredicates); ok {
		for _, p := range wp.GetPredicates() {
			d := predicateDiameter(p)
			if d > nodeMax {
				nodeMax = d
			}
		}
	}
	// Recurse into children, take max.
	childMax := 0
	for _, q := range expr.GetQuantifiers() {
		ref := q.GetRangesOver()
		if ref == nil {
			continue
		}
		for _, m := range ref.Members() {
			c := EvaluatePredicateComplexity(m)
			if c > childMax {
				childMax = c
			}
		}
	}
	if childMax > nodeMax {
		return childMax
	}
	return nodeMax
}

// predicateDiameter computes the "diameter with level counting" of a
// predicate tree. This is the maximum number of children at any level
// of the tree — Java's TreeLike.diameterWithLevelCounting.
//
// For a leaf predicate, the diameter is 1 (the node itself).
// For an AND/OR/NOT node, the diameter is max(len(children), max
// child diameters).
func predicateDiameter(p predicates.QueryPredicate) int {
	if p == nil {
		return 0
	}
	children := p.Children()
	if len(children) == 0 {
		return 1
	}
	maxChildDiam := 0
	for _, c := range children {
		d := predicateDiameter(c)
		if d > maxChildDiam {
			maxChildDiam = d
		}
	}
	width := len(children)
	if maxChildDiam > width {
		return maxChildDiam
	}
	return width
}
