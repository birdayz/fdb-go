package values

// AliasMap maps old correlation identifiers to new ones. Used during
// plan construction when a quantifier's alias changes and downstream
// values need to reference the new alias.
type AliasMap map[CorrelationIdentifier]CorrelationIdentifier

// RebaseValue replaces correlation references in a value tree
// according to the alias map. Returns the original value if no
// references match. Handles QuantifiedObjectValue, FieldValue, and
// recursively processes composite values.
//
// Ports Java's Value.rebase(AliasMap).
func RebaseValue(v Value, aliases AliasMap) Value {
	if v == nil || len(aliases) == 0 {
		return v
	}
	switch val := v.(type) {
	case *QuantifiedObjectValue:
		if newAlias, ok := aliases[val.Correlation]; ok {
			return &QuantifiedObjectValue{
				Correlation: newAlias,
				Typ:         val.Typ,
			}
		}
		return v
	case *FieldValue:
		return v
	case *ConstantValue:
		return v
	case *NullValue:
		return v
	case *BooleanValue:
		return v
	case *ArithmeticValue:
		newLeft := RebaseValue(val.Left, aliases)
		newRight := RebaseValue(val.Right, aliases)
		if newLeft == val.Left && newRight == val.Right {
			return v
		}
		return &ArithmeticValue{
			Op:    val.Op,
			Left:  newLeft,
			Right: newRight,
		}
	default:
		return v
	}
}
