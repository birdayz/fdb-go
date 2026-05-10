package values

// IsFunctionallyDependentOn reports whether v is functionally dependent
// on otherValue — meaning v's output is fully determined by
// otherValue's output. Ports Java's Value.isFunctionallyDependentOn.
//
// Returns true if all QuantifiedObjectValue leaves in v reference the
// same correlation as otherValue (when otherValue is a QOV). Returns
// false for non-QOV otherValue.
func IsFunctionallyDependentOn(v Value, otherValue Value) bool {
	otherQOV, ok := otherValue.(*QuantifiedObjectValue)
	if !ok {
		return false
	}

	allDependent := true
	WalkValue(v, func(node Value) bool {
		if !allDependent {
			return false
		}
		if qov, isQOV := node.(*QuantifiedObjectValue); isQOV {
			if qov.Correlation != otherQOV.Correlation {
				allDependent = false
				return false
			}
		}
		return true
	})
	return allDependent
}

// WithComparison creates a ComparisonPredicate from a Value and a
// comparison. Convenience method matching Java's
// Value.withComparison(Comparison).
//
// Note: returns a predicates.ComparisonPredicate but the import is
// avoided by returning the predicate components. Callers construct
// the predicate in their own package.

// AsPlaceholder creates a Placeholder predicate from a Value and a
// parameter alias. Convenience method matching Java's
// Value.asPlaceholder(CorrelationIdentifier).
//
// Note: same import-avoidance as WithComparison — returns components.
