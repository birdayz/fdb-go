package values

// AliasMap maps old correlation identifiers to new ones. Used during
// plan construction when a quantifier's alias changes and downstream
// values need to reference the new alias.
type AliasMap map[CorrelationIdentifier]CorrelationIdentifier

// RebaseValue replaces correlation references in a value tree
// according to the alias map. Returns the original value if no
// references match.
//
// Leaf values with correlation aliases (QuantifiedObjectValue,
// QuantifiedRecordValue, ExistsValue, ScalarSubqueryValue,
// ObjectValue) have their alias remapped directly. All other
// non-leaf values recursively rebase children and reconstruct
// via WithChildren — no per-type wiring needed.
//
// Ports Java's Value.rebase(AliasMap): leaf values override
// rebaseLeaf(); non-leaf values use the default rebase() which
// recurses children and calls withChildren().
func RebaseValue(v Value, aliases AliasMap) Value {
	if v == nil || len(aliases) == 0 {
		return v
	}

	// Handle leaf values with correlation aliases first.
	switch val := v.(type) {
	case *QuantifiedObjectValue:
		if newAlias, ok := aliases[val.Correlation]; ok {
			return &QuantifiedObjectValue{
				Correlation: newAlias,
				Typ:         val.Typ,
			}
		}
		return v
	case *QuantifiedRecordValue:
		if newAlias, ok := aliases[val.Alias]; ok {
			return &QuantifiedRecordValue{
				Alias:      newAlias,
				ResultType: val.ResultType,
			}
		}
		return v
	case *ExistsValue:
		if newAlias, ok := aliases[val.Alias]; ok {
			return &ExistsValue{Alias: newAlias}
		}
		return v
	case *ScalarSubqueryValue:
		if newAlias, ok := aliases[val.Alias]; ok {
			return &ScalarSubqueryValue{Alias: newAlias}
		}
		return v
	case *ObjectValue:
		if newAlias, ok := aliases[val.Alias]; ok {
			return &ObjectValue{Alias: newAlias, ResultType: val.ResultType}
		}
		return v
	}

	// For all other leaf values (FieldValue, ConstantValue, NullValue,
	// BooleanValue, ParameterValue, etc.), no rebase needed.
	children := v.Children()
	if len(children) == 0 {
		return v
	}

	// Recursively rebase children.
	changed := false
	newChildren := make([]Value, len(children))
	for i, child := range children {
		newChildren[i] = RebaseValue(child, aliases)
		if newChildren[i] != child {
			changed = true
		}
	}
	if !changed {
		return v
	}

	return WithChildren(v, newChildren)
}
