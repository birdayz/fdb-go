package predicates

import "github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"

// RebasePredicate replaces correlation references in a predicate tree
// according to the alias map. Returns the original predicate if no
// references match. Handles ComparisonPredicate, AndPredicate,
// OrPredicate, NotPredicate, ValuePredicate, ConstantPredicate.
//
// Ports Java's QueryPredicate.rebase(AliasMap).
func RebasePredicate(p QueryPredicate, aliases values.AliasMap) QueryPredicate {
	if p == nil || len(aliases) == 0 {
		return p
	}
	switch pred := p.(type) {
	case *ComparisonPredicate:
		newOperand := values.RebaseValue(pred.Operand, aliases)
		newCompOperand := values.RebaseValue(pred.Comparison.Operand, aliases)
		if newOperand == pred.Operand && newCompOperand == pred.Comparison.Operand {
			return p
		}
		return &ComparisonPredicate{
			Operand: newOperand,
			Comparison: Comparison{
				Type:    pred.Comparison.Type,
				Operand: newCompOperand,
				Escape:  pred.Comparison.Escape,
			},
		}
	case *ValuePredicate:
		newVal := values.RebaseValue(pred.Value, aliases)
		if newVal == pred.Value {
			return p
		}
		return NewValuePredicate(newVal)
	case *AndPredicate:
		return rebaseNary(pred, pred.SubPredicates, aliases, func(subs []QueryPredicate) QueryPredicate {
			return NewAnd(subs...)
		})
	case *OrPredicate:
		return rebaseNary(pred, pred.SubPredicates, aliases, func(subs []QueryPredicate) QueryPredicate {
			return NewOr(subs...)
		})
	case *NotPredicate:
		newChild := RebasePredicate(pred.Child, aliases)
		if newChild == pred.Child {
			return p
		}
		return NewNot(newChild)
	case *ConstantPredicate:
		return p
	default:
		return p
	}
}

func rebaseNary(orig QueryPredicate, subs []QueryPredicate, aliases values.AliasMap, build func([]QueryPredicate) QueryPredicate) QueryPredicate {
	changed := false
	newSubs := make([]QueryPredicate, len(subs))
	for i, s := range subs {
		newSubs[i] = RebasePredicate(s, aliases)
		if newSubs[i] != s {
			changed = true
		}
	}
	if !changed {
		return orig
	}
	return build(newSubs)
}
