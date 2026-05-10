package values

// IsFunctionallyDependentOn reports whether v is functionally dependent
// on otherValue — meaning v's output is fully determined by
// otherValue's output. Ports Java's Value.isFunctionallyDependentOn.
//
// Returns true if all correlation-bearing leaves in v reference the
// same correlation as otherValue (when otherValue is a QOV). Returns
// false if any leaf references a different scope, or if otherValue is
// not a QOV.
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
		// Check ALL correlation-bearing value types, not just QOV.
		switch n := node.(type) {
		case *QuantifiedObjectValue:
			if n.Correlation != otherQOV.Correlation {
				allDependent = false
			}
		case *QuantifiedRecordValue:
			if n.Alias != otherQOV.Correlation {
				allDependent = false
			}
		case *ExistsValue:
			if n.Alias != otherQOV.Correlation {
				allDependent = false
			}
		case *ScalarSubqueryValue:
			if n.Alias != otherQOV.Correlation {
				allDependent = false
			}
		}
		return allDependent
	})
	return allDependent
}
