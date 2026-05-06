package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
)

// PlannerConstraint is a typed key for constraints that flow between
// rules during the PLANNING phase. Rules read constraints set by their
// parent and push constraints to child References.
//
// Ports Java's PlannerConstraint.
type PlannerConstraint[T any] struct {
	name string
}

// RequestedOrderingConstraintKey is the constraint key for requested orderings.
var RequestedOrderingConstraintKey = &PlannerConstraint[[]*RequestedOrdering]{name: "requestedOrdering"}

// ConstraintMap holds constraints per Reference. Rules read constraints
// from the map and push new constraints for child References.
type ConstraintMap struct {
	constraints map[constraintEntry]any
}

type constraintEntry struct {
	ref *expressions.Reference
	key any
}

// NewConstraintMap creates an empty constraint map.
func NewConstraintMap() *ConstraintMap {
	return &ConstraintMap{constraints: make(map[constraintEntry]any)}
}

// Get retrieves the constraint value for a Reference + key combination.
func Get[T any](cm *ConstraintMap, ref *expressions.Reference, key *PlannerConstraint[T]) (T, bool) {
	if cm == nil {
		var zero T
		return zero, false
	}
	v, ok := cm.constraints[constraintEntry{ref: ref, key: key}]
	if !ok {
		var zero T
		return zero, false
	}
	return v.(T), true
}

// Set stores a constraint value for a Reference + key combination.
func Set[T any](cm *ConstraintMap, ref *expressions.Reference, key *PlannerConstraint[T], value T) {
	if cm == nil {
		return
	}
	cm.constraints[constraintEntry{ref: ref, key: key}] = value
}
