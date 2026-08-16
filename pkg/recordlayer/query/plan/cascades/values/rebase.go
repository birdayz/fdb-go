package values

// RebaseValue IS DELIBERATELY ABSENT, for the same reason RebasePredicate is
// (predicates/rebase.go): it was the error-less spelling of
// RebaseValueChecked and returned nil on failure, which is not failing closed.
//
// Nil is a legitimate value at most of the eleven production call sites this
// had, and at three of them it was actively dangerous. In match_info_merge.go
// the enclosing function answers (Value, bool) where `nil, true` is the LAWFUL
// "this cannot be pulled up" reply — so a failed rebase was indistinguishable
// from a legal decline and silently dropped an index-match subsumption. In
// match_max_match_map.go nil is already the "no match map" sentinel. The
// remaining sites had an error channel available and simply were not using it.
//
// RFC-232 is what took the failure from theoretical to reachable: it originates
// in reconstruction, and exact types are precisely what gave reconstruction
// something to reject. Deleting the wrapper is the fix rather than fixing its
// callers one at a time, because the next caller wanting "the one without the
// error return" would otherwise find it.
//
// Use RebaseValueChecked and route the error. Tests that expect success have
// mustRebaseValue helpers in their own packages.

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
