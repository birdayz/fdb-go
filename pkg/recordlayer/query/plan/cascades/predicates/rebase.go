package predicates

import "fdb.dev/pkg/recordlayer/query/plan/cascades/values"

// RebasePredicate is the compatibility, error-less spelling of
// RebasePredicateChecked. It fails closed with nil when exact Value
// reconstruction fails.
func RebasePredicate(p QueryPredicate, aliases values.AliasMap) QueryPredicate {
	rebased, err := RebasePredicateChecked(p, aliases)
	if err != nil {
		return nil
	}
	return rebased
}

// RebasePredicateChecked replaces correlation references in every embedded
// Value tree through the checked rewrite authority, then rebases the one
// correlation stored outside a Value (Placeholder.ParameterAlias). The whole
// predicate graph is invocation-local and atomic: an invalid FieldValue
// reconstruction returns an error and no partial predicate.
func RebasePredicateChecked(p QueryPredicate, aliases values.AliasMap) (QueryPredicate, error) {
	if p == nil {
		return p, nil
	}
	rebased, err := TransformEmbeddedValuesChecked(p, func(value values.Value) (values.Value, error) {
		return values.RebaseValueChecked(value, aliases)
	})
	if err != nil {
		return nil, err
	}
	return rebasePredicateMetadata(rebased, aliases), nil
}

func rebasePredicateMetadata(p QueryPredicate, aliases values.AliasMap) QueryPredicate {
	switch pred := p.(type) {
	case *AndPredicate:
		return rebasePredicateMetadataNary(pred, pred.SubPredicates, aliases, func(subs []QueryPredicate) QueryPredicate {
			return NewAnd(subs...)
		})
	case *OrPredicate:
		return rebasePredicateMetadataNary(pred, pred.SubPredicates, aliases, func(subs []QueryPredicate) QueryPredicate {
			return NewOr(subs...)
		})
	case *NotPredicate:
		newChild := rebasePredicateMetadata(pred.Child, aliases)
		if newChild == pred.Child {
			return p
		}
		return NewNot(newChild)
	case *Placeholder:
		newAlias := pred.ParameterAlias
		if aliases != nil {
			if mapped, ok := aliases.Target(newAlias); ok {
				newAlias = mapped
			}
		}
		if newAlias == pred.ParameterAlias {
			return p
		}
		return &Placeholder{
			ParameterAlias: newAlias,
			Value:          pred.Value,
			CompRange:      pred.CompRange,
		}
	default:
		return p
	}
}

func rebasePredicateMetadataNary(orig QueryPredicate, subs []QueryPredicate, aliases values.AliasMap, build func([]QueryPredicate) QueryPredicate) QueryPredicate {
	changed := false
	newSubs := make([]QueryPredicate, len(subs))
	for i, s := range subs {
		newSubs[i] = rebasePredicateMetadata(s, aliases)
		if newSubs[i] != s {
			changed = true
		}
	}
	if !changed {
		return orig
	}
	return build(newSubs)
}
