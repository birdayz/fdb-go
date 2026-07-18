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
// Set pushes a constraint with Java pushProperty semantics (RFC-181
// WS-P stage (b) first commit): the per-key LATTICE COMBINE decides —
// an absent key stores the push; a present key stores the combined
// value when the lattice grew and SUBSUMES the push otherwise (no
// store, no tick — Java's empty Optional). Both stores see the same
// combined value: the planner-global map (read-authoritative) and the
// per-Reference epoch mirror (which ticks only on real change, so an
// unchanged re-Set per rule re-fire can never hold a group
// unconverged once epochs drive convergence). The former plain
// overwrite ALSO silently clobbered a shared child's accumulated
// referenced fields when two parents pushed different sets — the
// union combine is the Java-faithful repair.
func Set[T any](cm *ConstraintMap, ref *expressions.Reference, key *PlannerConstraint[T], value T) {
	if cm == nil {
		return
	}
	combine := combineForKey(key)
	entry := constraintEntry{ref: ref.Canonical(), key: key}
	stored := any(value)
	if existing, ok := cm.constraints[entry]; ok {
		combined, changed := combine(existing, any(value))
		if !changed {
			// Subsumed: nothing to store, no epoch tick.
			return
		}
		stored = combined
	}
	cm.constraints[entry] = stored
	ref.ConstraintsMap().PushProperty(key, stored, combine)
}

// combineForKey returns the per-key lattice combine (Java dispatches
// through PlannerConstraint.combine): orderings use the
// subsumption-aware union, referenced fields the set union. An unknown
// key is conservatively always-changed (over-ticking errs toward
// re-exploration, never toward missing a push).
func combineForKey(key any) func(existing, pushed any) (any, bool) {
	switch key {
	case any(RequestedOrderingConstraintKey):
		return func(existing, pushed any) (any, bool) {
			cur, _ := existing.([]*RequestedOrdering)
			add, _ := pushed.([]*RequestedOrdering)
			return CombineRequestedOrderings(cur, add)
		}
	case any(ReferencedFieldsConstraintKey):
		return func(existing, pushed any) (any, bool) {
			cur, _ := existing.(*ReferencedFields)
			add, _ := pushed.(*ReferencedFields)
			return CombineReferencedFields(cur, add)
		}
	}
	return func(_, pushed any) (any, bool) { return pushed, true }
}
