package cascades

import "fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"

// Scheduling may omit an inert propagation only before an earlier preorder
// task could change its source or destination constraints.
type constraintPropagationRule interface {
	hasConstraintEffect(*ConstraintMap, *expressions.Reference, expressions.RelationalExpression) bool
}

func passThroughConstraintHasEffect[T any](cm *ConstraintMap, ref *expressions.Reference, expr expressions.RelationalExpression, key *PlannerConstraint[T]) bool {
	value, present := Get(cm, ref, key)
	if !present {
		return false
	}
	qs := expr.GetQuantifiers()
	if len(qs) == 0 || qs[0].GetRangesOver() == nil {
		return false
	}
	current, present := Get(cm, qs[0].GetRangesOver(), key)
	if !present {
		return true
	}
	_, changed := combineForKey(key)(current, value)
	return changed
}

// preOrderMarker is an embeddable type that marks an ImplementationRule
// as a PreOrder rule. PreOrder rules fire BEFORE child exploration in
// the unified task-stack. They push constraints (orderings, referenced
// fields) to child References.
type preOrderMarker struct{}

func (preOrderMarker) IsPreOrder() bool { return true }
