package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
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

// ReferencedFieldsConstraintKey is the constraint key for referenced
// fields. Pushed top-down by PushReferencedFieldsThrough* rules to
// inform downstream operators which columns/fields are actually needed.
// Ports Java's ReferencedFieldsConstraint.REFERENCED_FIELDS.
var ReferencedFieldsConstraintKey = &PlannerConstraint[*ReferencedFields]{name: "referencedFields"}

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
// The Reference is canonicalized: a constraint pushed on a since-merged
// alias must stay visible to a reader holding the survivor (and vice
// versa) — the map's identity is the GROUP, not the pointer.
func Get[T any](cm *ConstraintMap, ref *expressions.Reference, key *PlannerConstraint[T]) (T, bool) {
	if cm == nil {
		var zero T
		return zero, false
	}
	v, ok := cm.constraints[constraintEntry{ref: ref.Canonical(), key: key}]
	if !ok {
		var zero T
		return zero, false
	}
	return v.(T), true
}

// Set stores a constraint value for a Reference + key combination (the
// Reference canonicalized — see Get).
func Set[T any](cm *ConstraintMap, ref *expressions.Reference, key *PlannerConstraint[T], value T) {
	if cm == nil {
		return
	}
	cm.constraints[constraintEntry{ref: ref.Canonical(), key: key}] = value
	// Mirror the push into the Reference's tick/watermark ConstraintsMap
	// (RFC-181 WS-P stage (a)). NOT every Set is a real push:
	// PushConstraint pre-combines, but rule_push_referenced_fields
	// re-Sets an UNCHANGED value on every re-fire — so this mirror
	// OVER-TICKS relative to Java's pushProperty (which subsumes
	// unchanged pushes without a tick). Harmless while nothing reads
	// the epochs for control; stage (b)'s FIRST commit must route the
	// real lattice combine through PushProperty (subsumption = no
	// tick), or an unchanged re-Set per round would keep the epoch
	// unconverged forever once epochs drive convergence. The
	// planner-global map stays authoritative for reads until then.
	ref.ConstraintsMap().PushProperty(key, value, func(_, pushed any) (any, bool) { return pushed, true })
}
