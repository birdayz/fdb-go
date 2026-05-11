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
	case *PredicateWithValueAndRanges:
		if folded := foldPredicateWithRanges(q); folded != nil {
			return folded
		}
		return q
	}
	return p
}

// foldPredicateWithRanges implements Java's
// ConstantFoldingPredicateWithRangesRule + ConstantFoldingMultiConstraintPredicateRule.
// Folds a PredicateWithValueAndRanges to a ConstantPredicate when the LHS value
// and comparison operands are boolean/null constants.
func foldPredicateWithRanges(p *PredicateWithValueAndRanges) QueryPredicate {
	if len(p.ranges) != 1 {
		return nil
	}
	rc := p.ranges[0]
	comps := rc.GetComparisons()
	if len(comps) == 0 {
		return nil
	}
	if len(comps) == 1 {
		return foldSingleComparison(p.value, comps[0])
	}
	// Multi-constraint: fold each comparison, then AND the results.
	var results []TriBool
	for _, c := range comps {
		folded := foldSingleComparison(p.value, c)
		if folded == nil {
			return nil
		}
		cp, ok := folded.(*ConstantPredicate)
		if !ok {
			return nil
		}
		results = append(results, cp.Value)
	}
	combined := TriTrue
	for _, r := range results {
		combined = triBoolAnd(combined, r)
	}
	return &ConstantPredicate{Value: combined}
}

func foldSingleComparison(lhsValue values.Value, comp Comparison) QueryPredicate {
	lhs := effectiveConstant(lhsValue)
	if lhs == ecUnknown {
		return nil
	}

	if comp.Type == ComparisonIsNull {
		switch lhs {
		case ecNull:
			return &ConstantPredicate{Value: TriTrue}
		case ecTrue, ecFalse, ecNotNull:
			return &ConstantPredicate{Value: TriFalse}
		default:
			return nil
		}
	}
	if comp.Type == ComparisonIsNotNull {
		switch lhs {
		case ecNull:
			return &ConstantPredicate{Value: TriFalse}
		case ecTrue, ecFalse, ecNotNull:
			return &ConstantPredicate{Value: TriTrue}
		default:
			return nil
		}
	}

	if comp.Type != ComparisonEquals && comp.Type != ComparisonNotEquals {
		return nil
	}

	var rhs effectiveConstantKind
	if comp.Operand != nil {
		rhs = effectiveConstant(comp.Operand)
	} else {
		rhs = ecUnknown
	}
	if rhs == ecUnknown {
		return nil
	}

	if lhs == ecNull || rhs == ecNull {
		return &ConstantPredicate{Value: TriUnknown}
	}
	if lhs == ecNotNull || rhs == ecNotNull {
		return nil
	}

	if comp.Type == ComparisonEquals {
		if lhs == rhs {
			return &ConstantPredicate{Value: TriTrue}
		}
		return &ConstantPredicate{Value: TriFalse}
	}
	// NOT_EQUALS
	if lhs != rhs {
		return &ConstantPredicate{Value: TriTrue}
	}
	return &ConstantPredicate{Value: TriFalse}
}

type effectiveConstantKind int

const (
	ecTrue effectiveConstantKind = iota
	ecFalse
	ecNull
	ecNotNull
	ecUnknown
)

func effectiveConstant(v values.Value) effectiveConstantKind {
	if v == nil {
		return ecNull
	}
	if _, ok := v.(*values.NullValue); ok {
		return ecNull
	}
	if bv, ok := v.(*values.BooleanValue); ok {
		if bv.Value == nil {
			return ecNull
		}
		if *bv.Value {
			return ecTrue
		}
		return ecFalse
	}
	if cv, ok := v.(*values.ConstantValue); ok {
		if cv.Value == nil {
			return ecNull
		}
		if b, ok := cv.Value.(bool); ok {
			if b {
				return ecTrue
			}
			return ecFalse
		}
		return ecNotNull
	}
	return ecUnknown
}

func triBoolAnd(a, b TriBool) TriBool {
	if a == TriFalse || b == TriFalse {
		return TriFalse
	}
	if a == TriUnknown || b == TriUnknown {
		return TriUnknown
	}
	return TriTrue
}
