// Portions derived from FoundationDB Record Layer (PlannerConstraint.java,
// ReferencedFieldsConstraint.java),
// Copyright 2015-2021 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package cascades

import (
	"fmt"
	"slices"
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// PlannerConstraint is a typed key for constraints that flow between
// rules during the PLANNING phase. Rules read constraints set by their
// parent and push constraints to child References.
//
// Ports Java's PlannerConstraint.
type PlannerConstraint[T any] struct {
	name          string
	optimizerOnly bool
}

// AffectsExploration excludes retention requirements that no expression rule reads.
func (c *PlannerConstraint[T]) AffectsExploration() bool {
	return !c.optimizerOnly
}

// RequestedOrderingConstraintKey is the constraint key for requested orderings.
var RequestedOrderingConstraintKey = &PlannerConstraint[[]*properties.RequestedOrdering]{name: "requestedOrdering"}

// OrdinalLayoutConstraintKey accumulates the exact physical input layouts
// required by finalized parents of a group. OptimizeInputs pushes one
// positional requirement per child edge; OptimizeGroup retains the cheapest
// final satisfying every accumulated requirement instead of allowing the
// group's single global winner to erase a costlier compatible alternative.
var OrdinalLayoutConstraintKey = &PlannerConstraint[[]plans.OrdinalLayoutRequirement]{name: "ordinalLayout", optimizerOnly: true}

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
// The returned verdict reports whether the lattice GREW — Java's
// pushProperty Optional presence. Callers gate re-exploration
// scheduling on it (a subsumed push schedules nothing).
func Set[T any](cm *ConstraintMap, ref *expressions.Reference, key *PlannerConstraint[T], value T) bool {
	if cm == nil {
		return false
	}
	return cm.push(ref, key, any(value))
}

func (cm *ConstraintMap) push(ref *expressions.Reference, key, value any) bool {
	combine := combineForKey(key)
	entry := constraintEntry{ref: ref.Canonical(), key: key}
	stored := value
	if existing, ok := cm.constraints[entry]; ok {
		combined, changed := combine(existing, value)
		if !changed {
			// Subsumed: nothing to store, no epoch tick.
			return false
		}
		stored = combined
	}
	cm.constraints[entry] = stored
	ref.ConstraintsMap().PushProperty(key, stored, combine)
	return true
}

// constraintValue is one key's constraint on one group.
type constraintValue struct {
	key   any
	value any
}

// constraintsOf returns ref's constraints in key-name order. Entries are
// keyed by the group that was canonical when they were pushed.
func (cm *ConstraintMap) constraintsOf(ref *expressions.Reference) []constraintValue {
	if cm == nil {
		return nil
	}
	var result []constraintValue
	for entry, value := range cm.constraints {
		if entry.ref == ref {
			result = append(result, constraintValue{key: entry.key, value: value})
		}
	}
	slices.SortFunc(result, func(a, b constraintValue) int {
		return strings.Compare(constraintName(a.key), constraintName(b.key))
	})
	return result
}

// rehome moves loser's constraints onto survivor through the per-key lattice
// combine, so the merged group answers every parent of both groups. Returns
// loser's constraints as they were.
func (cm *ConstraintMap) rehome(loser, survivor *expressions.Reference) []constraintValue {
	moved := cm.constraintsOf(loser)
	for _, constraint := range moved {
		delete(cm.constraints, constraintEntry{ref: loser, key: constraint.key})
		cm.push(survivor, constraint.key, constraint.value)
	}
	return moved
}

func constraintName(key any) string {
	if named, ok := key.(interface{ constraintName() string }); ok {
		return named.constraintName()
	}
	return fmt.Sprintf("%T", key)
}

func (c *PlannerConstraint[T]) constraintName() string { return c.name }

func init() {
	// Register the typed lattice dispatch for constraint folds performed
	// inside the expressions package (Memo Absorb).
	expressions.SetConstraintCombineProvider(combineForKey)
}

// combineForKey returns the per-key lattice combine (Java dispatches
// through PlannerConstraint.combine): orderings use the
// subsumption-aware union, referenced fields the set union, and ordinal
// layouts the semantic requirement union. An unknown key is conservatively
// always-changed (over-ticking errs toward re-exploration, never toward
// missing a push).
func combineForKey(key any) func(existing, pushed any) (any, bool) {
	switch key {
	case any(RequestedOrderingConstraintKey):
		return func(existing, pushed any) (any, bool) {
			cur, _ := existing.([]*properties.RequestedOrdering)
			add, _ := pushed.([]*properties.RequestedOrdering)
			return properties.CombineRequestedOrderings(cur, add)
		}
	case any(ReferencedFieldsConstraintKey):
		return func(existing, pushed any) (any, bool) {
			cur, _ := existing.(*ReferencedFields)
			add, _ := pushed.(*ReferencedFields)
			return CombineReferencedFields(cur, add)
		}
	case any(OrdinalLayoutConstraintKey):
		return func(existing, pushed any) (any, bool) {
			cur, _ := existing.([]plans.OrdinalLayoutRequirement)
			add, _ := pushed.([]plans.OrdinalLayoutRequirement)
			return combineOrdinalLayoutRequirements(cur, add)
		}
	}
	return func(_, pushed any) (any, bool) { return pushed, true }
}

// combineOrdinalLayoutRequirements is an order-preserving set union. The
// requirement objects are sealed immutable views; only the containing slice
// needs copying when the lattice grows. Nil requirements are not valid plan
// properties and are ignored defensively at this generic constraint boundary.
func combineOrdinalLayoutRequirements(
	current, added []plans.OrdinalLayoutRequirement,
) ([]plans.OrdinalLayoutRequirement, bool) {
	fresh := make([]plans.OrdinalLayoutRequirement, 0, len(added))
	for _, candidate := range added {
		if candidate == nil {
			continue
		}
		duplicate := false
		for _, existing := range current {
			if plans.OrdinalLayoutRequirementsEqual(existing, candidate) {
				duplicate = true
				break
			}
		}
		if !duplicate {
			for _, existing := range fresh {
				if plans.OrdinalLayoutRequirementsEqual(existing, candidate) {
					duplicate = true
					break
				}
			}
		}
		if !duplicate {
			fresh = append(fresh, candidate)
		}
	}
	if len(fresh) == 0 {
		return current, false
	}
	combined := make([]plans.OrdinalLayoutRequirement, 0, len(current)+len(fresh))
	combined = append(combined, current...)
	combined = append(combined, fresh...)
	return combined, true
}
