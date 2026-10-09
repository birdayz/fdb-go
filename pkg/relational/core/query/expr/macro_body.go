package expr

import (
	"errors"
	"fmt"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/functions"
	antlrgen "fdb.dev/pkg/relational/core/parser/gen"
)

// swappedComparison is RelOpValue.swapBinaryComparisonOperator.
var swappedComparison = map[predicates.ComparisonType]predicates.ComparisonType{
	predicates.ComparisonEquals: predicates.ComparisonEquals, predicates.ComparisonNotEquals: predicates.ComparisonNotEquals,
	predicates.ComparisonIsDistinctFrom: predicates.ComparisonIsDistinctFrom, predicates.ComparisonNotDistinctFrom: predicates.ComparisonNotDistinctFrom,
	predicates.ComparisonLessThan: predicates.ComparisonGreaterThan, predicates.ComparisonGreaterThan: predicates.ComparisonLessThan,
	predicates.ComparisonLessThanOrEq: predicates.ComparisonGreaterThanEq, predicates.ComparisonGreaterThanEq: predicates.ComparisonLessThanOrEq,
}

// relOpComparisons are the predicate comparisons Java builds as a RelOpValue
// (SqlFunctionCatalogImpl: =, <>, <, <=, >, >=, IS [NOT] NULL, IS [NOT]
// DISTINCT FROM).
var relOpComparisons = map[predicates.ComparisonType]values.RelOpComparison{}

func init() {
	for v, p := range predicates.RelOpComparisonType {
		relOpComparisons[p] = v
	}
}

// WalkMacroBody is a macro body as the Value tree Java's ExpressionVisitor
// builds, and UserDefinedMacroFunction persists, for it. A comparison is a
// RelOpValue over its operands as written (RelOpValue.encapsulate), AND/OR an
// AndOrValue, NOT a NotValue, IN an InOpValue over an `__internal_array`,
// BETWEEN its two comparisons, LIKE a LikeOperatorValue and a searched CASE a
// PickValue over a ConditionSelectorValue (ExpressionVisitor.java:498-812). Go
// resolves those shapes as predicates, rewritten for planning, which neither
// persist nor match Java's bytes; lowerMacroBody resolves the tree that way
// when the macro is called. Any other body is the ordinary projection walk.
func (r *Resolver) WalkMacroBody(e antlrgen.IExpressionContext) (values.Value, error) {
	if isMacroBooleanShape(e) {
		return r.macroValue(e)
	}
	return r.WalkExpressionForProjection(e)
}

// isMacroBooleanShape is an expression WalkMacroBody builds itself.
func isMacroBooleanShape(e antlrgen.IExpressionContext) bool {
	switch c := e.(type) {
	case *antlrgen.LogicalExpressionContext, *antlrgen.NotExpressionContext, *antlrgen.ExistsExpressionAtomContext:
		return true
	case *antlrgen.PredicatedExpressionContext:
		return c.Predicate() != nil || isMacroBooleanAtom(c.ExpressionAtom())
	}
	return false
}

func isMacroBooleanAtom(a antlrgen.IExpressionAtomContext) bool {
	switch c := a.(type) {
	case *antlrgen.BinaryComparisonPredicateContext:
		return true
	case *antlrgen.FunctionCallExpressionAtomContext:
		return searchedCase(c) != nil
	case *antlrgen.RecordConstructorExpressionAtomContext:
		inner := parenthesizedExpression(c)
		return inner != nil && isMacroBooleanShape(inner)
	}
	return false
}

func searchedCase(c *antlrgen.FunctionCallExpressionAtomContext) *antlrgen.CaseFunctionCallContext {
	if sfc, ok := c.FunctionCall().(*antlrgen.SpecificFunctionCallContext); ok {
		if cs, ok := sfc.SpecificFunction().(*antlrgen.CaseFunctionCallContext); ok {
			return cs
		}
	}
	return nil
}

// parenthesizedExpression is the one unnamed expression of `(e)`, nil for
// any other record constructor.
func parenthesizedExpression(c *antlrgen.RecordConstructorExpressionAtomContext) antlrgen.IExpressionContext {
	rc, ok := c.RecordConstructor().(*antlrgen.RecordConstructorContext)
	if !ok || rc.OfTypeClause() != nil {
		return nil
	}
	items := rc.AllExpressionWithOptionalName()
	if len(items) != 1 {
		return nil
	}
	item, ok := items[0].(*antlrgen.ExpressionWithOptionalNameContext)
	if !ok || item.AS() != nil {
		return nil
	}
	return item.Expression()
}

func (r *Resolver) macroValue(e antlrgen.IExpressionContext) (values.Value, error) {
	switch c := e.(type) {
	case *antlrgen.LogicalExpressionContext:
		lo, ok := c.LogicalOperator().(*antlrgen.LogicalOperatorContext)
		if !ok || len(c.AllExpression()) != 2 {
			return nil, &UnsupportedExpressionShapeError{Shape: "LogicalExpression"}
		}
		left, err := r.macroOperand(c.Expression(0))
		if err != nil {
			return nil, err
		}
		right, err := r.macroOperand(c.Expression(1))
		if err != nil {
			return nil, err
		}
		switch {
		case lo.AND() != nil || len(lo.AllBIT_AND_OP()) == 2:
			return values.NewAndOrValue(values.AndOrAnd, left, right), nil
		case lo.OR() != nil || len(lo.AllBIT_OR_OP()) == 2:
			return values.NewAndOrValue(values.AndOrOr, left, right), nil
		}
		return nil, macroUnsupported("XOR")
	case *antlrgen.ExistsExpressionAtomContext:
		// Java stores ExistsValue over the subquery's quantifier but not the
		// subquery, which the function's plan fragment drops: the stored body
		// cannot be called.
		return nil, macroUnsupported("EXISTS")
	case *antlrgen.NotExpressionContext:
		child, err := r.macroOperand(c.Expression())
		if err != nil {
			return nil, err
		}
		return values.NewNotValue(child), nil
	case *antlrgen.PredicatedExpressionContext:
		operand, err := r.macroAtom(c.ExpressionAtom())
		if err != nil {
			return nil, err
		}
		if c.Predicate() == nil {
			return operand, nil
		}
		return r.macroPredicate(operand, c.Predicate())
	}
	return r.WalkExpressionForProjection(e)
}

// macroOperand is an argument of a built-in: resolveFunction flattens a
// single-item record argument (BaseVisitor.java:253-261).
func (r *Resolver) macroOperand(e antlrgen.IExpressionContext) (values.Value, error) {
	var v values.Value
	var err error
	if isMacroBooleanShape(e) {
		v, err = r.macroValue(e)
	} else {
		v, err = r.walkExpressionInner(e, posOperand)
	}
	if err != nil {
		return nil, err
	}
	return functions.FlattenRecordWithOneField(v), nil
}

func (r *Resolver) macroAtomOperand(a antlrgen.IExpressionAtomContext) (values.Value, error) {
	v, err := r.macroAtom(a)
	if err != nil {
		return nil, err
	}
	return functions.FlattenRecordWithOneField(v), nil
}

func (r *Resolver) macroAtom(a antlrgen.IExpressionAtomContext) (values.Value, error) {
	switch c := a.(type) {
	case *antlrgen.BinaryComparisonPredicateContext:
		op, err := comparisonOpFromCtx(c.ComparisonOperator())
		if err != nil {
			return nil, err
		}
		left, err := r.macroAtomOperand(c.GetLeft())
		if err != nil {
			return nil, err
		}
		right, err := r.macroAtomOperand(c.GetRight())
		if err != nil {
			return nil, err
		}
		return relOp(relOpComparisons[op], left, right)
	case *antlrgen.FunctionCallExpressionAtomContext:
		if cs := searchedCase(c); cs != nil {
			return r.macroCase(cs)
		}
	case *antlrgen.RecordConstructorExpressionAtomContext:
		if inner := parenthesizedExpression(c); inner != nil && isMacroBooleanShape(inner) {
			return r.macroValue(inner)
		}
	}
	return r.walkAtomInner(a, posOperand)
}

func (r *Resolver) macroPredicate(operand values.Value, p antlrgen.IPredicateContext) (values.Value, error) {
	operand = functions.FlattenRecordWithOneField(operand)
	switch c := p.(type) {
	case *antlrgen.IsExpressionContext:
		switch {
		case c.NULL_LITERAL() != nil && c.NOT() != nil:
			return relOp(values.RelOpNotNull, operand, nil)
		case c.NULL_LITERAL() != nil:
			return relOp(values.RelOpIsNull, operand, nil)
		}
		// visitIsExpression: `x IS [NOT] b` is `x IS NOT NULL AND x = b`, or
		// negated `x IS NULL OR x = NOT b`.
		literal := c.TRUE() != nil
		nullCheck, combine := values.RelOpNotNull, values.AndOrAnd
		if c.NOT() != nil {
			literal, nullCheck, combine = !literal, values.RelOpIsNull, values.AndOrOr
		}
		check, err := relOp(nullCheck, operand, nil)
		if err != nil {
			return nil, err
		}
		eq, err := relOp(values.RelOpEquals, operand, values.NewBooleanValue(literal))
		if err != nil {
			return nil, err
		}
		return values.NewAndOrValue(combine, check, eq), nil
	case *antlrgen.InPredicateContext:
		il, ok := c.InList().(*antlrgen.InListContext)
		if !ok || il.QueryExpressionBody() != nil {
			return nil, api.NewError(api.ErrCodeUnsupportedQuery, "IN predicate does not support nested SELECT")
		}
		var array values.Value
		if ec, ok := il.Expressions().(*antlrgen.ExpressionsContext); ok && ec != nil {
			items := make([]values.Value, 0, len(ec.AllExpression()))
			for _, e := range ec.AllExpression() {
				if IsBareNullLiteral(e) {
					return nil, &InListNullError{}
				}
				item, err := r.macroOperand(e)
				if err != nil {
					return nil, err
				}
				items = append(items, item)
			}
			arr, err := values.NewLightArrayConstructorValue(items)
			if err != nil {
				return nil, incompatible(err)
			}
			array = arr
		} else if fcn := il.FullColumnName(); fcn != nil {
			col, err := r.walkColumnRef(fcn.FullId())
			if err != nil {
				return nil, err
			}
			if _, isArray := col.Type().(*values.ArrayType); !isArray {
				return nil, api.NewErrorf(api.ErrCodeUnsupportedQuery,
					"IN list with column reference must be of array type, but got: %v", col.Type())
			}
			array = col
		} else {
			return nil, macroUnsupported("IN over a parameter")
		}
		in, err := values.NewJavaInOpValue(operand, array)
		if err != nil {
			return nil, incompatible(err)
		}
		if c.NOT() != nil {
			return values.NewNotValue(in), nil
		}
		return in, nil
	case *antlrgen.LikePredicateContext:
		pattern, err := r.walkConstant(c.GetPattern())
		if err != nil {
			return nil, err
		}
		// An absent ESCAPE is `new LiteralValue<>(null)`: a NULL-typed literal.
		escape := values.Value(&values.ConstantValue{Typ: values.NullType})
		if tok := c.GetEscape(); tok != nil {
			escape = &values.ConstantValue{Value: stripStringLiteral(tok.GetText()), Typ: values.NotNullString}
		}
		patternValue, err := values.NewPatternForLikeValueChecked(pattern, escape)
		if err != nil {
			return nil, api.NewError(api.ErrCodeInvalidArgumentForFunction, err.Error())
		}
		like, err := values.NewLikeOperatorValueChecked(operand, patternValue)
		if err != nil {
			return nil, api.NewError(api.ErrCodeInvalidArgumentForFunction, err.Error())
		}
		if c.NOT() != nil {
			return values.NewNotValue(like), nil
		}
		return like, nil
	case *antlrgen.BetweenComparisonPredicateContext:
		lo, err := r.macroAtomOperand(c.GetLeft())
		if err != nil {
			return nil, err
		}
		hi, err := r.macroAtomOperand(c.GetRight())
		if err != nil {
			return nil, err
		}
		if c.NOT() == nil {
			lower, err := relOp(values.RelOpLessThanOrEquals, lo, operand)
			if err != nil {
				return nil, err
			}
			upper, err := relOp(values.RelOpLessThanOrEquals, operand, hi)
			if err != nil {
				return nil, err
			}
			return values.NewAndOrValue(values.AndOrAnd, lower, upper), nil
		}
		below, err := relOp(values.RelOpLessThan, operand, lo)
		if err != nil {
			return nil, err
		}
		above, err := relOp(values.RelOpGreaterThan, operand, hi)
		if err != nil {
			return nil, err
		}
		return values.NewAndOrValue(values.AndOrOr, below, above), nil
	}
	return nil, &UnsupportedExpressionShapeError{Shape: fmt.Sprintf("grammar Predicate %T", p)}
}

// macroCase is visitCaseFunctionCall: a PickValue over the conditions, an
// ELSE selected by TautologicalValue.
func (r *Resolver) macroCase(c *antlrgen.CaseFunctionCallContext) (values.Value, error) {
	var implications, alternatives []values.Value
	arg := func(fa antlrgen.IFunctionArgContext) (values.Value, error) {
		fac, ok := fa.(*antlrgen.FunctionArgContext)
		if !ok || fac.Expression() == nil {
			return nil, &UnsupportedExpressionShapeError{Shape: "CASE argument"}
		}
		return r.macroOperand(fac.Expression())
	}
	for _, alt := range c.AllCaseFuncAlternative() {
		ac := alt.(*antlrgen.CaseFuncAlternativeContext)
		cond, err := arg(ac.GetCondition())
		if err != nil {
			return nil, err
		}
		if t := cond.Type(); t == nil || t.Code() != values.TypeCodeBoolean {
			return nil, api.NewError(api.ErrCodeDatatypeMismatch, "argument of case when must be of boolean type")
		}
		cons, err := arg(ac.GetConsequent())
		if err != nil {
			return nil, err
		}
		implications = append(implications, cond)
		alternatives = append(alternatives, cons)
	}
	if c.ELSE() != nil {
		def, err := arg(c.GetElseArg())
		if err != nil {
			return nil, err
		}
		implications = append(implications, values.TautologicalValue{})
		alternatives = append(alternatives, def)
	}
	pick, err := values.NewJavaPickValue(values.NewConditionSelectorValue(implications), alternatives)
	if err != nil {
		return nil, incompatible(err)
	}
	return pick, nil
}

func relOp(comparison values.RelOpComparison, left, right values.Value) (values.Value, error) {
	var v values.Value
	var err error
	if right == nil {
		v, err = values.NewUnaryRelOpValue(comparison, left)
	} else {
		v, err = values.NewBinaryRelOpValue(comparison, left, right)
	}
	var relErr *values.RelOpError
	if errors.As(err, &relErr) {
		if relErr.Complex {
			return nil, api.NewError(api.ErrCodeUnsupportedQuery, "a comparison operand of complex type (record) is not supported")
		}
		return nil, api.NewError(api.ErrCodeDatatypeMismatch, "The operands of a comparison operator are not compatible.")
	}
	return v, err
}

func incompatible(err error) error {
	return api.NewError(api.ErrCodeDatatypeMismatch, err.Error())
}

func macroUnsupported(shape string) error {
	return api.NewErrorf(api.ErrCodeUnsupportedOperation, "%s cannot be persisted in a function body", shape)
}

// lowerMacroBody is the expanded body as Go resolves the same expression
// written inline: each RelOpValue, AndOrValue, NotValue, InOpValue and CASE
// becomes what the resolver builds for its text, so a macro plans and
// evaluates exactly as its body would.
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
		op, left, right := predicates.RelOpComparisonType[vv.Comparison], lowered[0], lowered[1]
		// RelOpValue.toQueryPredicate puts the correlated operand on the left
		// (swapBinaryComparisonOperator): `2 <= x` compares x.
		if len(values.GetCorrelatedToOfValue(left)) == 0 && len(values.GetCorrelatedToOfValue(right)) > 0 {
			op, left, right = swappedComparison[op], right, left
		}
		pred, err := r.ResolveComparison(op, left, right)
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
			return &predicateValue{pred: r.ResolveOr(flattenOr(left, right)...)}, nil
		}
		return &predicateValue{pred: r.ResolveAnd(flattenAnd(left, right)...)}, nil
	case *values.NotValue:
		if pv, ok := lowered[0].(*predicateValue); ok {
			return &predicateValue{pred: r.ResolveNot(pv.pred)}, nil
		}
	case *values.InOpValue:
		if items, literal, ok := inListItems(lowered[1]); ok {
			pred, err := r.resolveInList(unpromoted(lowered[0]), items, literal)
			if err != nil {
				return nil, err
			}
			return &predicateValue{pred: pred}, nil
		}
	case *values.LikeOperatorValue:
		if pattern, ok := lowered[1].(*values.PatternForLikeValue); ok {
			pred, err := r.ResolveLike(lowered[0], pattern.PatternChild, pattern.EscapeChild)
			if err != nil {
				return nil, err
			}
			return &predicateValue{pred: pred}, nil
		}
	case values.TautologicalValue:
		return values.NewBooleanValue(true), nil
	case *values.ConditionSelectorValue:
		// walkCaseCondition: a condition is the predicate it resolves to.
		for i, impl := range lowered {
			if _, isPred := impl.(*predicateValue); isPred {
				continue
			}
			if b, isBool := impl.(*values.BooleanValue); isBool && i == len(lowered)-1 && b.Value != nil && *b.Value {
				if _, wasElse := vv.Implications[i].(values.TautologicalValue); wasElse {
					continue
				}
			}
			pred, err := r.liftValueToPredicate(impl)
			if err != nil {
				return nil, err
			}
			lowered[i] = &predicateValue{pred: pred}
			changed = true
		}
		return values.NewConditionSelectorValue(lowered), nil
	case *values.PickValue:
		// walkCaseFunctionCall types the branches as written.
		alternatives := make([]values.Value, len(lowered)-1)
		for i, alt := range lowered[1:] {
			alternatives[i] = unpromoted(alt)
		}
		typ := caseResultType(alternatives)
		return values.NewPickValue(lowered[0], promoteTemporalBranches(alternatives, typ), typ), nil
	case *values.ConstantValue:
		if vv.Value == nil && vv.Typ != nil && vv.Typ.Code() == values.TypeCodeNull {
			return values.NewNullValue(vv.Typ), nil
		}
	}
	if !changed {
		return v, nil
	}
	return values.WithChildrenChecked(v, lowered)
}

// inListItems are the items of an IN list built by `__internal_array`, as
// written: the resolver injects its own promotions.
func inListItems(list values.Value) ([]values.Value, bool, bool) {
	arr, ok := unpromoted(list).(*values.ArrayConstructorValue)
	if !ok {
		return nil, false, false
	}
	items := make([]values.Value, len(arr.Elements))
	literal := true
	for i, e := range arr.Elements {
		items[i] = unpromoted(e)
		switch items[i].(type) {
		case *values.ConstantValue, *values.BooleanValue:
		default:
			literal = false
		}
	}
	return items, literal, true
}

func unpromoted(v values.Value) values.Value {
	if p, ok := v.(*values.PromoteValue); ok {
		return p.Child
	}
	return v
}

// conditionOf is a lowered operand of AND/OR as a predicate.
func (r *Resolver) conditionOf(v values.Value) (predicates.QueryPredicate, error) {
	if pv, ok := v.(*predicateValue); ok {
		return pv.pred, nil
	}
	return r.liftValueToPredicate(v)
}
