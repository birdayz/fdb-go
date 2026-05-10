package cascades

import "github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"

// ValueSemanticEquals compares two Values for semantic equality using
// the given ValueEquivalence for cross-scope comparisons. Returns a
// ConstrainedBoolean that may carry a QueryPlanConstraint.
//
// The algorithm:
//  1. Pointer equality → always true.
//  2. EqualsWithoutChildren + recursive child comparison via
//     ValueSemanticEquals (composing constraints).
//  3. If structural comparison fails, falls back to
//     valueEquivalence.IsDefinedEqual (axiom-based equality).
//
// Ports Java's Value.semanticEquals(other, ValueEquivalence).
func ValueSemanticEquals(a, b values.Value, veq ValueEquivalence) ConstrainedBoolean {
	if a == b {
		return AlwaysTrue()
	}
	if a == nil || b == nil {
		return FalseValue()
	}

	typed := valueSemanticEqualsTyped(a, b, veq)
	if typed.IsFalse() {
		return veq.IsDefinedEqual(a, b)
	}
	return typed
}

// valueSemanticEqualsTyped does the structural comparison:
// equalsWithoutChildren + recursive children comparison.
// Ports Java's Value.semanticEqualsTyped.
func valueSemanticEqualsTyped(a, b values.Value, veq ValueEquivalence) ConstrainedBoolean {
	if !values.EqualsWithoutChildren(a, b) {
		return FalseValue()
	}

	ac := a.Children()
	bc := b.Children()
	if len(ac) != len(bc) {
		return FalseValue()
	}

	result := AlwaysTrue()
	for i := range ac {
		childResult := ValueSemanticEquals(ac[i], bc[i], veq)
		if childResult.IsFalse() {
			return FalseValue()
		}
		result = result.ComposeWithOther(childResult)
	}
	return result
}
