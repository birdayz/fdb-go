package predicates

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// SimplifyPredicateValues walks a QueryPredicate tree and returns a new
// tree with SimplifyValue applied to every Value operand reachable
// inside ComparisonPredicate / ValuePredicate leaves. Boolean
// connectives (AND / OR / NOT) recurse into their children.
//
// Returns the input pointer unchanged when nothing folded — callers
// can rely on the pointer-equality short-circuit (`if out != p { ... }`)
// to detect "did anything change?".
//
// Why a separate pass from Simplify: Simplify drives the QueryPredicate-
// level rule fixpoint (ComparisonConstantSimplifyRule, AndFlatten, …);
// it doesn't fold expression-level constants inside ComparisonPredicate
// operands. `name = 1+2` survives Simplify with the `1+2` ArithmeticValue
// intact; SimplifyPredicateValues collapses it to `name = 3`. Phase 4.6
// merges this into the ValueSimplificationRuleSet.
func SimplifyPredicateValues(p QueryPredicate) QueryPredicate {
	if p == nil {
		return nil
	}
	switch q := p.(type) {
	case *ComparisonPredicate:
		op := values.SimplifyValue(q.Operand)
		var rhs values.Value
		if q.Comparison.Operand != nil {
			rhs = values.SimplifyValue(q.Comparison.Operand)
		}
		if op == q.Operand && rhs == q.Comparison.Operand {
			return q
		}
		return &ComparisonPredicate{
			Operand: op,
			Comparison: Comparison{
				Type:    q.Comparison.Type,
				Operand: rhs,
				Escape:  q.Comparison.Escape,
			},
		}
	case *ValuePredicate:
		v := values.SimplifyValue(q.Value)
		if v == q.Value {
			return q
		}
		return &ValuePredicate{Value: v}
	case *AndPredicate:
		simpler := make([]QueryPredicate, len(q.SubPredicates))
		anyChanged := false
		for i, sp := range q.SubPredicates {
			simpler[i] = SimplifyPredicateValues(sp)
			if simpler[i] != sp {
				anyChanged = true
			}
		}
		if !anyChanged {
			return q
		}
		return &AndPredicate{SubPredicates: simpler}
	case *OrPredicate:
		simpler := make([]QueryPredicate, len(q.SubPredicates))
		anyChanged := false
		for i, sp := range q.SubPredicates {
			simpler[i] = SimplifyPredicateValues(sp)
			if simpler[i] != sp {
				anyChanged = true
			}
		}
		if !anyChanged {
			return q
		}
		return &OrPredicate{SubPredicates: simpler}
	case *NotPredicate:
		c := SimplifyPredicateValues(q.Child)
		if c == q.Child {
			return q
		}
		return &NotPredicate{Child: c}
	}
	// Unknown predicate shape (BooleanConstantPredicate, future
	// additions): pass through. Adding new shapes is mechanical when
	// the need arises.
	return p
}
