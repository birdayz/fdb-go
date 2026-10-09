package expr

import (
	"errors"
	"fmt"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/api"
)

// relOpComparisons are the predicate comparisons Java builds as a RelOpValue
// (SqlFunctionCatalogImpl: =, <>, <, <=, >, >=, IS [NOT] NULL, IS [NOT]
// DISTINCT FROM).
var relOpComparisons = map[predicates.ComparisonType]values.RelOpComparison{}

func init() {
	for v, p := range predicates.RelOpComparisonType {
		relOpComparisons[p] = v
	}
}

// MacroBodyValue is a macro body as the Value tree Java's ExpressionVisitor
// builds and persists for it: a comparison is a RelOpValue
// (RelOpValue.encapsulate), AND/OR an AndOrValue, NOT a NotValue and LIKE a
// LikeOperatorValue. Go resolves a boolean expression as a predicate, which
// cannot be persisted, so the resolved predicate is turned back into that
// tree; lowerMacroBody undoes it when the macro is called.
func MacroBodyValue(v values.Value) (values.Value, error) {
	switch vv := v.(type) {
	case *predicateValue:
		return predicateAsJavaValue(vv.pred)
	case *values.NotValue:
		child, err := MacroBodyValue(vv.Child)
		if err != nil {
			return nil, err
		}
		return values.NewNotValue(child), nil
	case *values.AndOrValue:
		left, err := MacroBodyValue(vv.Left)
		if err != nil {
			return nil, err
		}
		right, err := MacroBodyValue(vv.Right)
		if err != nil {
			return nil, err
		}
		return values.NewAndOrValue(vv.Op, left, right), nil
	}
	return v, nil
}

func predicateAsJavaValue(p predicates.QueryPredicate) (values.Value, error) {
	switch pp := p.(type) {
	case *predicates.ConstantPredicate:
		switch {
		case pp.Value == nil:
			return values.NewNullValue(values.NullableBoolean), nil
		case *pp.Value:
			return values.NewBooleanValue(true), nil
		}
		return values.NewBooleanValue(false), nil
	case *predicates.AndPredicate:
		return foldAndOr(values.AndOrAnd, pp.SubPredicates)
	case *predicates.OrPredicate:
		return foldAndOr(values.AndOrOr, pp.SubPredicates)
	case *predicates.NotPredicate:
		child, err := predicateAsJavaValue(pp.Child)
		if err != nil {
			return nil, err
		}
		return values.NewNotValue(child), nil
	case *predicates.ComparisonPredicate:
		return comparisonAsJavaValue(pp)
	}
	return nil, fmt.Errorf("a %T cannot be persisted in a function body", p)
}

// foldAndOr nests left-deep, as the binary grammar rule parses `a AND b AND c`.
func foldAndOr(op values.AndOrOp, subs []predicates.QueryPredicate) (values.Value, error) {
	var out values.Value
	for _, sub := range subs {
		v, err := predicateAsJavaValue(sub)
		if err != nil {
			return nil, err
		}
		if out == nil {
			out = v
		} else {
			out = values.NewAndOrValue(op, out, v)
		}
	}
	if out == nil {
		return nil, fmt.Errorf("an empty %s cannot be persisted in a function body", op)
	}
	return out, nil
}

func comparisonAsJavaValue(cp *predicates.ComparisonPredicate) (values.Value, error) {
	lhs, err := MacroBodyValue(cp.Operand)
	if err != nil {
		return nil, err
	}
	switch cp.Comparison.Type {
	case predicates.ComparisonLike:
		if pattern, ok := cp.Comparison.Operand.(*values.PatternForLikeValue); ok {
			return values.NewLikeOperatorValue(lhs, pattern), nil
		}
	case predicates.ComparisonIsNull, predicates.ComparisonIsNotNull:
		return relOpValueOrError(values.NewUnaryRelOpValue(relOpComparisons[cp.Comparison.Type], lhs))
	}
	comparison, ok := relOpComparisons[cp.Comparison.Type]
	if !ok || cp.Comparison.Operand == nil {
		return nil, fmt.Errorf("a comparison of type %v cannot be persisted in a function body", cp.Comparison.Type)
	}
	rhs, err := MacroBodyValue(cp.Comparison.Operand)
	if err != nil {
		return nil, err
	}
	// A bare boolean used as a condition was lifted to `b = TRUE`
	// (Expression.Utils.toUnderlyingPredicate); Java keeps the value itself.
	if b, isBool := rhs.(*values.BooleanValue); isBool && comparison == values.RelOpEquals &&
		b.Value != nil && *b.Value && lhs.Type() != nil && lhs.Type().Code() == values.TypeCodeBoolean {
		return lhs, nil
	}
	// Java's encapsulate types the operator over the operands as written; the
	// promotions Go's resolver injected are redone by toQueryPredicate.
	if rel, err := values.NewBinaryRelOpValue(comparison, unpromoted(lhs), unpromoted(rhs)); err == nil {
		return rel, nil
	}
	return relOpValueOrError(values.NewBinaryRelOpValue(comparison, lhs, rhs))
}

func unpromoted(v values.Value) values.Value {
	if p, ok := v.(*values.PromoteValue); ok {
		return p.Child
	}
	return v
}

func relOpValueOrError[T values.Value](v T, err error) (values.Value, error) {
	var relErr *values.RelOpError
	if errors.As(err, &relErr) {
		return nil, api.NewError(api.ErrCodeDatatypeMismatch, "The operands of a comparison operator are not compatible.")
	}
	if err != nil {
		return nil, err
	}
	return v, nil
}

// lowerMacroBody is the expanded body as Go resolves the same expression
// written inline: each RelOpValue, AndOrValue and NotValue over a condition
// becomes the predicate its toQueryPredicate builds, through the resolver, so
// a macro plans and evaluates exactly as its body text would.
func (r *Resolver) lowerMacroBody(v values.Value) (values.Value, error) {
	if v == nil {
		return nil, nil
	}
	if _, ok := v.(*predicateValue); ok {
		return v, nil
	}
	children := v.Children()
	lowered := make([]values.Value, len(children))
	changed := false
	for i, child := range children {
		c, err := r.lowerMacroBody(child)
		if err != nil {
			return nil, err
		}
		lowered[i] = c
		changed = changed || c != child
	}
	switch vv := v.(type) {
	case *values.BinaryRelOpValue:
		pred, err := r.ResolveComparison(predicates.RelOpComparisonType[vv.Comparison], lowered[0], lowered[1])
		if err != nil {
			return nil, err
		}
		return &predicateValue{pred: pred}, nil
	case *values.UnaryRelOpValue:
		var pred predicates.QueryPredicate
		var err error
		if vv.Comparison == values.RelOpIsNull {
			pred, err = r.ResolveIsNull(lowered[0])
		} else {
			pred, err = r.ResolveIsNotNull(lowered[0])
		}
		if err != nil {
			return nil, err
		}
		return &predicateValue{pred: pred}, nil
	case *values.AndOrValue:
		left, err := r.conditionOf(lowered[0])
		if err != nil {
			return nil, err
		}
		right, err := r.conditionOf(lowered[1])
		if err != nil {
			return nil, err
		}
		if vv.Op == values.AndOrOr {
			return &predicateValue{pred: r.ResolveOr(left, right)}, nil
		}
		return &predicateValue{pred: r.ResolveAnd(left, right)}, nil
	case *values.NotValue:
		if pv, ok := lowered[0].(*predicateValue); ok {
			return &predicateValue{pred: r.ResolveNot(pv.pred)}, nil
		}
	}
	if !changed {
		return v, nil
	}
	return values.WithChildrenChecked(v, lowered)
}

// conditionOf is a lowered operand of AND/OR as a predicate.
func (r *Resolver) conditionOf(v values.Value) (predicates.QueryPredicate, error) {
	if pv, ok := v.(*predicateValue); ok {
		return pv.pred, nil
	}
	return r.liftValueToPredicate(v)
}
