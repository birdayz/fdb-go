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
	case *CastValue:
		newChild := RebaseValue(val.Child, aliases)
		if newChild == val.Child {
			return v
		}
		return &CastValue{Child: newChild, Target: val.Target}
	case *PromoteValue:
		newChild := RebaseValue(val.Child, aliases)
		if newChild == val.Child {
			return v
		}
		return &PromoteValue{Child: newChild, Target: val.Target}
	case *ScalarFunctionValue:
		changed := false
		newArgs := make([]Value, len(val.Args))
		for i, arg := range val.Args {
			newArgs[i] = RebaseValue(arg, aliases)
			if newArgs[i] != arg {
				changed = true
			}
		}
		if !changed {
			return v
		}
		return &ScalarFunctionValue{
			FuncName: val.FuncName,
			Args:     newArgs,
			Typ:      val.Typ,
		}
	case *RecordConstructorValue:
		changed := false
		newFields := make([]RecordConstructorField, len(val.Fields))
		for i, f := range val.Fields {
			newVal := RebaseValue(f.Value, aliases)
			newFields[i] = RecordConstructorField{Name: f.Name, Value: newVal}
			if newVal != f.Value {
				changed = true
			}
		}
		if !changed {
			return v
		}
		return &RecordConstructorValue{Fields: newFields}
	case *NotValue:
		newChild := RebaseValue(val.Child, aliases)
		if newChild == val.Child {
			return v
		}
		return &NotValue{Child: newChild}
	case *AggregateValue:
		if val.Operand == nil {
			return v
		}
		newOperand := RebaseValue(val.Operand, aliases)
		if newOperand == val.Operand {
			return v
		}
		return &AggregateValue{Op: val.Op, Operand: newOperand}
	case *ParameterValue:
		return v
	default:
		return v
	}
}
