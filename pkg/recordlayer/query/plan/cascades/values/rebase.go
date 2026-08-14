package values

// RebaseValue replaces correlation references in a value tree
// according to the alias map. Returns the original value if no
// references match.
//
// Leaf values with correlation aliases (QuantifiedObjectValue,
// QuantifiedRecordValue, ScalarSubqueryValue, ObjectValue) have their
// alias remapped directly. All other non-leaf values (including
// ExistsValue, a transparent composite over a QuantifiedObjectValue)
// recursively rebase children and reconstruct via WithChildren — no
// per-type wiring needed.
//
// Ports Java's Value.rebase(AliasMap): leaf values override
// rebaseLeaf(); non-leaf values use the default rebase() which
// recurses children and calls withChildren().
func RebaseValue(v Value, aliases AliasMap) Value {
	rebased, err := RebaseValueChecked(v, aliases)
	if err != nil {
		return nil
	}
	return rebased
}

// RebaseValueChecked performs the alias-only rebase through the same checked
// reconstruction authority used by TranslationMap. It preserves exact QOV
// types, rejects current-kind changes through the validated AliasMap, and
// returns no original/partial tree when an enclosing FieldValue cannot be
// rebuilt.
func RebaseValueChecked(v Value, aliases AliasMap) (Value, error) {
	if v == nil || aliasMapEmpty(aliases) {
		return v, nil
	}
	validated, ok := asAliasMap(aliases)
	if !ok {
		return nil, resolutionError(RewriteInvalidTranslation, "rebase.aliases", "alias map is not values-owned")
	}
	return rebaseValueChecked(v, validated)
}

func rebaseValueChecked(v Value, validated *aliasMap) (Value, error) {
	if v == nil {
		return nil, resolutionError(RewriteNilReplacement, "rebase.value", "rebase encountered a nil Value")
	}

	// Handle leaf values with correlation aliases first.
	switch val := v.(type) {
	case *quantifiedObjectValue:
		if newAlias, ok := validated.Target(val.correlation); ok {
			return &quantifiedObjectValue{
				correlation:  newAlias,
				flowed:       val.flowed,
				sourceLayout: val.sourceLayout,
			}, nil
		}
		return v, nil
	case *QuantifiedRecordValue:
		if newAlias, ok := validated.Target(val.Alias); ok {
			return &QuantifiedRecordValue{
				Alias:      newAlias,
				ResultType: val.ResultType,
			}, nil
		}
		return v, nil
	case *ScalarSubqueryValue:
		if newAlias, ok := validated.Target(val.Alias); ok {
			return &ScalarSubqueryValue{Alias: newAlias, Typ: val.Typ}, nil
		}
		return v, nil
	case *ObjectValue:
		if newAlias, ok := validated.Target(val.Alias); ok {
			return &ObjectValue{Alias: newAlias, ResultType: val.ResultType}, nil
		}
		return v, nil
	case *UnmatchedAggregateValue:
		if newAlias, ok := validated.Target(val.UnmatchedID); ok {
			return &UnmatchedAggregateValue{UnmatchedID: newAlias}, nil
		}
		return v, nil
	case *ConstantObjectValue:
		if newAlias, ok := validated.Target(val.Alias); ok {
			return &ConstantObjectValue{Alias: newAlias, ConstantID: val.ConstantID, ResultType: val.ResultType}, nil
		}
		return v, nil
	}

	// For all other leaf values (FieldValue, ConstantValue, NullValue,
	// BooleanValue, ParameterValue, etc.), no rebase needed.
	children := v.Children()
	if len(children) == 0 {
		return v, nil
	}

	// Recursively rebase children.
	changed := false
	newChildren := make([]Value, len(children))
	for i, child := range children {
		var err error
		newChildren[i], err = rebaseValueChecked(child, validated)
		if err != nil {
			return nil, err
		}
		if newChildren[i] != child {
			changed = true
		}
	}
	if !changed {
		return v, nil
	}

	return withChildrenChecked(v, newChildren)
}
