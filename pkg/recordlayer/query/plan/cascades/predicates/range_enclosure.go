// Portions derived from FoundationDB Record Layer (RangeConstraints.java),
// Copyright 2015-2023 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package predicates

import (
	"reflect"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// Encloses proves containment of literal-bound ranges. As in Java
// RangeConstraints.encloses, deferred candidate constraints prevent a proof;
// deferred query constraints may only narrow its known range. Runtime-bound
// parameters are deliberately not evaluated here: their values would require
// a captured plan-cache constraint, not an unconditional implication proof.
func (r *RangeConstraints) Encloses(other *RangeConstraints) bool {
	if r == nil || other == nil || len(r.deferredRanges) != 0 {
		return false
	}
	outer, ok := compileLiteralRange(r.compilableComparisons)
	if !ok {
		return false
	}
	inner, ok := compileLiteralRange(other.compilableComparisons)
	if !ok || inner.empty() {
		return false
	}
	if inner.null && !outer.null {
		return false
	}
	if !inner.nonNull {
		return true
	}
	if !outer.nonNull {
		return false
	}
	if outer.lower != nil {
		if inner.lower == nil {
			return false
		}
		cmp, ok := compareLiteralBounds(outer.lower.value, inner.lower.value)
		if !ok || cmp > 0 || (cmp == 0 && inner.lower.inclusive && !outer.lower.inclusive) {
			return false
		}
	}
	if outer.upper != nil {
		if inner.upper == nil {
			return false
		}
		cmp, ok := compareLiteralBounds(outer.upper.value, inner.upper.value)
		if !ok || cmp < 0 || (cmp == 0 && inner.upper.inclusive && !outer.upper.inclusive) {
			return false
		}
	}
	return true
}

type (
	literalRangeBound struct {
		value     any
		inclusive bool
	}
	literalRange struct {
		lower, upper  *literalRangeBound
		null, nonNull bool
	}
)

func (r literalRange) empty() bool { return !r.null && !r.nonNull }

// compileTimeComparand is CompilableRange.compile's
// comparison.getComparand(null, evaluationContext): Java evaluates the comparand
// VALUE, so `col2 IS NOT DISTINCT FROM CAST(3 AS BIGINT)` bounds the range as
// `= 3` does and implies a sparse index's `col2 IS NOT NULL`
// (ISCAN(I4 [NOT_DISTINCT_FROM CAST(@c12 AS LONG)])). Only a tree with no row
// and no binding evaluates; ConstantObjectValue and parameters stay declined,
// for the plan-cache reason Encloses gives.
func compileTimeComparand(v values.Value) (any, bool) {
	if constant, ok := v.(*values.ConstantValue); ok {
		return constant.Value, true
	}
	if !values.IsConstantValue(v) {
		return nil, false
	}
	out, err := v.Evaluate(nil)
	if err != nil {
		return nil, false
	}
	return out, true
}

func compileLiteralRange(comparisons []Comparison) (literalRange, bool) {
	r := literalRange{null: true, nonNull: true}
	for _, c := range comparisons {
		if c.ParameterName != "" {
			return literalRange{}, false
		}
		if c.Type == ComparisonIsNull {
			r.nonNull = false
			continue
		}
		if c.Type == ComparisonIsNotNull {
			r.null = false
			continue
		}
		comparand, ok := compileTimeComparand(c.Operand)
		if !ok || comparand == nil {
			return literalRange{}, false
		}
		// Prove that this domain has an ordering before accepting even a
		// one-sided interval. cmpAny preserves SQL's signed-zero equality and
		// exact integer comparison, also used when the residual is evaluated.
		if _, ok := compareLiteralBounds(comparand, comparand); !ok {
			return literalRange{}, false
		}
		r.null = false // SQL ordered comparisons never accept NULL rows.
		bound := &literalRangeBound{value: comparand}
		var lower, upper *literalRangeBound
		switch c.Type {
		case ComparisonEquals, ComparisonNotDistinctFrom:
			bound.inclusive = true
			lower, upper = bound, bound
		case ComparisonGreaterThan:
			lower = bound
		case ComparisonGreaterThanEq:
			bound.inclusive = true
			lower = bound
		case ComparisonLessThan:
			upper = bound
		case ComparisonLessThanOrEq:
			bound.inclusive = true
			upper = bound
		default:
			return literalRange{}, false
		}
		if lower != nil {
			if r.lower == nil {
				r.lower = lower
			} else {
				cmp, ok := compareLiteralBounds(lower.value, r.lower.value)
				if !ok {
					return literalRange{}, false
				}
				if cmp > 0 || (cmp == 0 && !lower.inclusive) {
					r.lower = lower
				}
			}
		}
		if upper != nil {
			if r.upper == nil {
				r.upper = upper
			} else {
				cmp, ok := compareLiteralBounds(upper.value, r.upper.value)
				if !ok {
					return literalRange{}, false
				}
				if cmp < 0 || (cmp == 0 && !upper.inclusive) {
					r.upper = upper
				}
			}
		}
	}
	if r.lower != nil && r.upper != nil {
		cmp, ok := compareLiteralBounds(r.lower.value, r.upper.value)
		if !ok {
			return literalRange{}, false
		}
		if cmp > 0 || (cmp == 0 && (!r.lower.inclusive || !r.upper.inclusive)) {
			r.nonNull = false
		}
	}
	return r, true
}

// Implication cannot use cross-domain coercion: rounding an integer to a
// float can turn a weaker query bound into an apparently equal bound.
func compareLiteralBounds(a, b any) (int, bool) {
	if c, ok := values.CompareExactInts(a, b); ok {
		return c, true
	}
	if reflect.TypeOf(a) != reflect.TypeOf(b) {
		return 0, false
	}
	return cmpAny(a, b)
}
