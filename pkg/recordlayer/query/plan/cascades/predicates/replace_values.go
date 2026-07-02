package predicates

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// ReplaceValues applies a Value replacement function to all Value trees
// embedded in a predicate, rebuilding only the spine that changed. Ports
// Java's QueryPredicate.replaceValuesMaybe. Pointer-equality stable: returns
// p itself when nothing changed.
//
// This is the EXPORTED home of the walk (moved from the cascades package's
// replacePredicateValues, which now delegates here): the RFC-173 Slice 2
// translator bakes gated-join leg references at the seed with it, and the
// planner's rule-side callers share the identical spine so predicate-value
// rewrites can never diverge between the two layers.
func ReplaceValues(p QueryPredicate, fn func(values.Value) values.Value) QueryPredicate {
	if p == nil {
		return nil
	}
	switch pred := p.(type) {
	case *ComparisonPredicate:
		newOperand := values.Replace(pred.Operand, fn)
		newCompOperand := values.Replace(pred.Comparison.Operand, fn)
		if newOperand == pred.Operand && newCompOperand == pred.Comparison.Operand {
			return p
		}
		// Copy the whole Comparison and replace ONLY the new RHS operand,
		// preserving Escape AND every other Comparison subclass field
		// (ParameterName, the Text* fields, the DistanceRank vector fields).
		// A partial {Type, Operand, Escape} reconstruction would drop the rest
		// and change the comparison's semantics.
		cmp := pred.Comparison
		cmp.Operand = newCompOperand
		return &ComparisonPredicate{
			Operand:    newOperand,
			Comparison: cmp,
		}
	case *ValuePredicate:
		newVal := values.Replace(pred.Value, fn)
		if newVal == pred.Value {
			return p
		}
		return NewValuePredicate(newVal)
	case *AndPredicate:
		changed := false
		newSubs := make([]QueryPredicate, len(pred.SubPredicates))
		for i, s := range pred.SubPredicates {
			newSubs[i] = ReplaceValues(s, fn)
			if newSubs[i] != s {
				changed = true
			}
		}
		if !changed {
			return p
		}
		return NewAnd(newSubs...)
	case *OrPredicate:
		changed := false
		newSubs := make([]QueryPredicate, len(pred.SubPredicates))
		for i, s := range pred.SubPredicates {
			newSubs[i] = ReplaceValues(s, fn)
			if newSubs[i] != s {
				changed = true
			}
		}
		if !changed {
			return p
		}
		return NewOr(newSubs...)
	case *NotPredicate:
		newChild := ReplaceValues(pred.Child, fn)
		if newChild == pred.Child {
			return p
		}
		return NewNot(newChild)
	case *Placeholder:
		newVal := values.Replace(pred.Value, fn)
		if newVal == pred.Value {
			return p
		}
		return &Placeholder{
			ParameterAlias: pred.ParameterAlias,
			Value:          newVal,
			CompRange:      pred.CompRange,
		}
	default:
		return p
	}
}
