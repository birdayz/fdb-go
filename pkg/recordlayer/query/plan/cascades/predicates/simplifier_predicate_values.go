package predicates

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
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
// It is the value half of Java's ValuePredicateSimplificationRule, which
// cascades.ConstantFoldingRules applies to each leaf inside the
// QueryPredicate-level fixpoint: `name = 1+2` becomes `name = 3`.
func SimplifyPredicateValues(p QueryPredicate) QueryPredicate {
	return mapPredicateLeafValues(p, values.SimplifyPredicateValue)
}

// EvaluatePredicateComparands is SimplifyPredicateValues with
// values.EvaluateConstantComparand at the leaves: every constant composite
// becomes a literal. It is for a predicate STORED with literal comparands --
// the sparse-index predicate, Java's IndexComparison taking
// `comparison.getComparand(null, null)` -- never for a query plan.
func EvaluatePredicateComparands(p QueryPredicate) QueryPredicate {
	return mapPredicateLeafValues(p, values.EvaluateConstantComparand)
}

func mapPredicateLeafValues(p QueryPredicate, leaf func(values.Value) values.Value) QueryPredicate {
	if p == nil {
		return nil
	}
	switch q := p.(type) {
	case *ComparisonPredicate:
		op := leaf(q.Operand)
		var rhs values.Value
		if q.Comparison.Operand != nil {
			rhs = leaf(q.Comparison.Operand)
		}
		if op == q.Operand && rhs == q.Comparison.Operand {
			return q
		}
		// Copy the whole Comparison and replace ONLY the simplified RHS operand,
		// preserving Escape AND every other Comparison subclass field
		// (ParameterName, the Text* fields, the DistanceRank vector fields).
		// A partial {Type, Operand, Escape} reconstruction would drop the rest
		// and change the comparison's semantics.
		cmp := q.Comparison
		cmp.Operand = rhs
		return &ComparisonPredicate{
			Operand:    op,
			Comparison: cmp,
		}
	case *ValuePredicate:
		v := leaf(q.Value)
		if v == q.Value {
			return q
		}
		return &ValuePredicate{Value: v}
	case *AndPredicate:
		simpler := make([]QueryPredicate, len(q.SubPredicates))
		anyChanged := false
		for i, sp := range q.SubPredicates {
			simpler[i] = mapPredicateLeafValues(sp, leaf)
			if simpler[i] != sp {
				anyChanged = true
			}
		}
		if !anyChanged {
			return q
		}
		return &AndPredicate{SubPredicates: simpler, atomic: q.atomic}
	case *OrPredicate:
		simpler := make([]QueryPredicate, len(q.SubPredicates))
		anyChanged := false
		for i, sp := range q.SubPredicates {
			simpler[i] = mapPredicateLeafValues(sp, leaf)
			if simpler[i] != sp {
				anyChanged = true
			}
		}
		if !anyChanged {
			return q
		}
		return &OrPredicate{SubPredicates: simpler, atomic: q.atomic}
	case *NotPredicate:
		c := mapPredicateLeafValues(q.Child, leaf)
		if c == q.Child {
			return q
		}
		return &NotPredicate{Child: c, atomic: q.atomic}
	case *PredicateWithValueAndRanges:
		simplified := TransformEmbeddedValues(q, leaf).(*PredicateWithValueAndRanges)
		if folded := foldPredicateWithRanges(simplified); folded != nil {
			return folded
		}
		return simplified
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
		return FoldComparisonMaybe(p.value, comps[0])
	}
	combined := TriTrue
	unknown := false
	for _, c := range comps {
		folded := FoldComparisonMaybe(p.value, c)
		cp, ok := folded.(*ConstantPredicate)
		if !ok {
			unknown = true
			continue
		}
		if cp.Value == TriFalse {
			return cp
		}
		combined = triBoolAnd(combined, cp.Value)
	}
	if unknown {
		return nil
	}
	return &ConstantPredicate{Value: combined}
}

// FoldComparisonMaybe is Java's ConstantPredicateFoldingUtil.foldComparisonMaybe:
// the constant a comparison of lhsValue folds to over EFFECTIVE constants, or
// nil when it does not fold. IS [NOT] NULL is decided by a NULL or a NOT NULL
// operand; a binary comparison with a NULL side is NULL; EQUALS and NOT_EQUALS
// over two known literals (TRUE, FALSE, NULL) compare them; anything else,
// including a comparison of two non-boolean literals, does not fold.
func FoldComparisonMaybe(lhsValue values.Value, comp Comparison) QueryPredicate {
	lhs := effectiveConstant(lhsValue)

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

	if comp.ParameterName != "" || comp.Operand == nil {
		return nil
	}
	rhs := effectiveConstant(comp.Operand)
	switch comp.Type {
	case ComparisonEquals, ComparisonNotEquals, ComparisonLessThan, ComparisonLessThanOrEq,
		ComparisonGreaterThan, ComparisonGreaterThanEq, ComparisonStartsWith:
		if lhs == ecNull || rhs == ecNull {
			return &ConstantPredicate{Value: TriUnknown}
		}
	default:
		return nil
	}
	if lhs == ecUnknown || rhs == ecUnknown || lhs == ecNotNull || rhs == ecNotNull {
		return nil
	}
	if comp.Type != ComparisonEquals && comp.Type != ComparisonNotEquals {
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

// effectiveConstant is Java's EffectiveConstant.from(Value)
// (ConstantPredicateFoldingUtil.java:282-301): a NullValue (or a nil / NULL
// literal) is NULL, a BOOLEAN literal is its value, and anything else is
// NOT_NULL when its type is NOT NULL and UNKNOWN otherwise. A non-boolean
// literal takes the type arm like every other value (a non-nil ConstantValue
// is typed NOT NULL, as a target literal is). Java's Object overload, for a
// SimpleComparison's literal comparand, has no Go planning caller.
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
	}
	if typ := v.Type(); typ != nil && !typ.IsNullable() {
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
