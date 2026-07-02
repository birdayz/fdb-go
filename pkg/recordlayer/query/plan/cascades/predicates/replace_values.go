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
	return transformEmbeddedValues(p, func(v values.Value) values.Value {
		return values.Replace(v, fn)
	})
}

// TranslateLeafPredicates applies a TranslationMap to every Value tree
// embedded in a predicate — the Go analog of Java's
// QueryPredicate.translateLeafPredicate (ValuePredicate.java:149: each
// predicate's embedded Value goes through Value.translateCorrelations).
// LEAF-ONLY by construction (the W2 pre-code ruling): the map fires on
// correlation-bearing leaves via values.TranslateCorrelations — never on
// interior nodes — and shares the exact predicate spine ReplaceValues uses,
// so the two rewrite families cannot diverge. Pointer-stable.
func TranslateLeafPredicates(p QueryPredicate, m values.TranslationMap) QueryPredicate {
	if m == nil || m.DefinesOnlyIdentities() {
		return p
	}
	return transformEmbeddedValues(p, func(v values.Value) values.Value {
		return values.TranslateCorrelations(v, m)
	})
}

// transformEmbeddedValues is the ONE predicate spine both value-rewrite
// families walk: transform is applied to each embedded Value tree WHOLE
// (operands, ValuePredicate value, Placeholder value), and only the changed
// spine is rebuilt.
func transformEmbeddedValues(p QueryPredicate, transform func(values.Value) values.Value) QueryPredicate {
	if p == nil {
		return nil
	}
	switch pred := p.(type) {
	case *ComparisonPredicate:
		newOperand := transform(pred.Operand)
		newCompOperand := transform(pred.Comparison.Operand)
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
		newVal := transform(pred.Value)
		if newVal == pred.Value {
			return p
		}
		return NewValuePredicate(newVal)
	case *AndPredicate:
		changed := false
		newSubs := make([]QueryPredicate, len(pred.SubPredicates))
		for i, s := range pred.SubPredicates {
			newSubs[i] = transformEmbeddedValues(s, transform)
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
			newSubs[i] = transformEmbeddedValues(s, transform)
			if newSubs[i] != s {
				changed = true
			}
		}
		if !changed {
			return p
		}
		return NewOr(newSubs...)
	case *NotPredicate:
		newChild := transformEmbeddedValues(pred.Child, transform)
		if newChild == pred.Child {
			return p
		}
		return NewNot(newChild)
	case *Placeholder:
		newVal := transform(pred.Value)
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
