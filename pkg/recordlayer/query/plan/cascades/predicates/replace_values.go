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
		newCmp, cmpChanged := transformComparison(pred.Comparison, transform)
		if newOperand == pred.Operand && !cmpChanged {
			return p
		}
		return &ComparisonPredicate{
			Operand:    newOperand,
			Comparison: newCmp,
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
	case *ExistentialValuePredicate:
		// Java ExistentialValuePredicate.translateLeafPredicate (:94-100)
		// translates the embedded existential QOV like any leaf. The rebuild
		// goes through the Must constructor: a transform that turns the QOV
		// into a non-QOV violates the predicate's structural invariant and
		// panics loudly there — never a silently mis-shaped predicate.
		// (Review catch: the arm was missing — the alias-map spine's
		// RebasePredicate covered this type while this spine fell to the
		// default silent skip.)
		newVal := transform(pred.Value)
		newCmp, cmpChanged := transformComparison(pred.Comparison, transform)
		if newVal == pred.Value && !cmpChanged {
			return p
		}
		return MustNewExistentialValuePredicate(newVal, newCmp)
	case *PredicateWithValueAndRanges:
		// Value-bearing leaf predicate (Java PredicateWithValueAndRanges
		// .translateLeafPredicate): the anchor value AND every range
		// comparison's embedded values rebase — a silent default-arm skip
		// left stale aliases after a cross-boundary rebase (review catch).
		newVal := transform(pred.GetValue())
		changed := newVal != pred.GetValue()
		newRanges := make([]*RangeConstraints, len(pred.GetRanges()))
		for i, rc := range pred.GetRanges() {
			newRC, rcChanged := transformRangeConstraints(rc, transform)
			newRanges[i] = newRC
			changed = changed || rcChanged
		}
		if !changed {
			return p
		}
		return NewPredicateWithValueAndRanges(newVal, newRanges)
	default:
		return p
	}
}

// transformComparison applies transform to every VALUE-BEARING field of a
// Comparison — the RHS Operand and the DistanceRank QueryVector (both are
// correlation surfaces per Comparison.GetCorrelatedTo; transforming only the
// Operand left stale aliases in vector predicates, review catch). Returns the
// (possibly copied) comparison and whether anything changed; all other
// subclass fields are preserved by the whole-struct copy.
func transformComparison(cmp Comparison, transform func(values.Value) values.Value) (Comparison, bool) {
	changed := false
	if cmp.Operand != nil {
		if newOp := transform(cmp.Operand); newOp != cmp.Operand {
			cmp.Operand = newOp
			changed = true
		}
	}
	if cmp.QueryVector != nil {
		if newQV := transform(cmp.QueryVector); newQV != cmp.QueryVector {
			cmp.QueryVector = newQV
			changed = true
		}
	}
	return cmp, changed
}

// transformRangeConstraints rebuilds a RangeConstraints with every
// comparison's value fields transformed; pointer-stable when nothing changed.
func transformRangeConstraints(rc *RangeConstraints, transform func(values.Value) values.Value) (*RangeConstraints, bool) {
	changed := false
	newCompilable := make([]Comparison, len(rc.GetCompilableComparisons()))
	for i, c := range rc.GetCompilableComparisons() {
		nc, cChanged := transformComparison(c, transform)
		newCompilable[i] = nc
		changed = changed || cChanged
	}
	newDeferred := make([]Comparison, len(rc.GetDeferredRanges()))
	for i, c := range rc.GetDeferredRanges() {
		nc, cChanged := transformComparison(c, transform)
		newDeferred[i] = nc
		changed = changed || cChanged
	}
	if !changed {
		return rc, false
	}
	return NewRangeConstraints(newCompilable, newDeferred), true
}
