package embedded

import (
	"context"
	"database/sql/driver"
	"fmt"
	"strings"

	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/functions"
	antlrgen "github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser/gen"
)

// Map-path predicate evaluators (mirror of eval_predicate.go).
//
// evalPredicateOnMap{Tri,Expr,ExprTri} are the WHERE / ON / HAVING
// evaluators for map-row representations (JOIN, CTE, post-aggregate).
// evalHaving / evalHavingTri are HAVING-specific entry points that
// handle aggregate-name resolution against post-aggregation rows.
// groupByKey lives here because it's the GROUP BY hash function the
// aggregate path uses to bucket map rows before evaluating HAVING.
//
// Mirrors evalPredicate / evalExprPredicate{,Tri} /
// evalComparisonPredicateTri / evalInPredicateTri /
// evalIsNullPredicate / evalLikePredicateTri /
// evalBetweenPredicateTri in eval_predicate.go. RFC-021 Phase 1c
// plans to merge the two paths behind a uniform Row interface —
// keeping them in parallel files makes the duplication visible.

// groupByKey builds a comparable string key from the group-by column values.
// Uses a type-tagged, length-prefixed encoding so that a NULL entry and the
// literal string "<nil>" produce different keys (fmt.Sprintf("%v", nil)
// would otherwise collide them), and so that values containing the
// separator byte cannot accidentally straddle adjacent columns. SQL groups
// NULLs together (NULL=NULL under GROUP BY), which is preserved because
// every NULL produces the same "N|" sentinel regardless of column type.
func groupByKey(groupVals []driver.Value) string {
	var b strings.Builder
	for _, v := range groupVals {
		if v == nil {
			b.WriteString("N|")
			continue
		}
		s := fmt.Sprintf("%T\x00%v", v, v)
		fmt.Fprintf(&b, "V:%d:%s|", len(s), s)
	}
	return b.String()
}

// evalHaving evaluates a HAVING clause expression against a map of
// output-column-name → aggregate value. Bool wrapper over evalHavingTri —
// UNKNOWN collapses to false at the filter boundary.
func evalHaving(ctx context.Context, conn *EmbeddedConnection, row map[string]driver.Value, expr antlrgen.IExpressionContext) (bool, error) {
	t, err := evalHavingTri(ctx, conn, row, expr)
	return t.IsTrue(), err
}

// evalHavingTri is the Kleene three-valued implementation for HAVING.
// Supports comparisons, AND/OR/NOT, and aggregate function references.
func evalHavingTri(ctx context.Context, conn *EmbeddedConnection, row map[string]driver.Value, expr antlrgen.IExpressionContext) (triBool, error) {
	// EXISTS subquery
	if exists, ok := expr.(*antlrgen.ExistsExpressionAtomContext); ok {
		if conn == nil {
			return triFalse, api.NewErrorf(api.ErrCodeUnsupportedOperation, "EXISTS subquery not supported in this context")
		}
		defer conn.pushOuterScope(outerScopeFromMapRow(row))()
		_, _, subRows, subErr := conn.execQueryBodyRows(ctx, exists.Query().QueryExpressionBody())
		if subErr != nil {
			return triFalse, subErr
		}
		return triFromBool(len(subRows) > 0), nil
	}
	// Handle logical expressions: AND / OR / XOR (+ symbolic forms).
	if le, ok := expr.(*antlrgen.LogicalExpressionContext); ok {
		left, err := evalHavingTri(ctx, conn, row, le.Expression(0))
		if err != nil {
			return triFalse, err
		}
		op := le.LogicalOperator()
		isAnd := op.AND() != nil || len(op.AllBIT_AND_OP()) >= 2
		isOr := op.OR() != nil || len(op.AllBIT_OR_OP()) >= 2
		isXor := op.XOR() != nil
		if isXor {
			right, err := evalHavingTri(ctx, conn, row, le.Expression(1))
			if err != nil {
				return triFalse, err
			}
			if left == triNull || right == triNull {
				return triNull, nil
			}
			return triFromBool((left == triTrue) != (right == triTrue)), nil
		}
		if isAnd {
			if left == triFalse {
				return triFalse, nil
			}
			right, err := evalHavingTri(ctx, conn, row, le.Expression(1))
			if err != nil {
				return triFalse, err
			}
			return triAnd(left, right), nil
		}
		if !isOr {
			return triFalse, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported logical operator %q", op.GetText())
		}
		// OR (including symbolic ||)
		if left == triTrue {
			return triTrue, nil
		}
		right, err := evalHavingTri(ctx, conn, row, le.Expression(1))
		if err != nil {
			return triFalse, err
		}
		return triOr(left, right), nil
	}
	// Handle NOT
	if ne, ok := expr.(*antlrgen.NotExpressionContext); ok {
		v, err := evalHavingTri(ctx, conn, row, ne.Expression())
		if err != nil {
			// On error the zero-value `v` is triFalse; v.Not() would return
			// triTrue and bury the error. Match evalExprPredicateTri NOT path.
			return triFalse, err
		}
		return v.Not(), nil
	}
	pred, ok := expr.(*antlrgen.PredicatedExpressionContext)
	if !ok {
		return triFalse, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported HAVING expression %T", expr)
	}
	// WHERE-style predicate: expressionAtom + separate predicate (IS NULL, LIKE, BETWEEN, IN, =).
	if pred.Predicate() != nil {
		return evalPredicateOnMapTri(ctx, conn, row, pred)
	}
	// Parenthesised HAVING: `HAVING (SUM(v) > 20)` parses the atom as a
	// RecordConstructorExpressionAtom with one unnamed expression. Unwrap
	// it and recurse on the inner expression so the rest of the HAVING
	// evaluator (comparison + logical ops) applies uniformly.
	if rc, isRC := pred.ExpressionAtom().(*antlrgen.RecordConstructorExpressionAtomContext); isRC {
		rec := rc.RecordConstructor()
		if rec == nil {
			return triFalse, api.NewErrorf(api.ErrCodeUnsupportedOperation, "empty record constructor in HAVING")
		}
		if rec.STAR() != nil || rec.OfTypeClause() != nil {
			return triFalse, api.NewErrorf(api.ErrCodeUnsupportedOperation, "HAVING does not support record constructor with STAR / OF TYPE")
		}
		fields := rec.AllExpressionWithOptionalName()
		if len(fields) == 1 && fields[0].AS() == nil && fields[0].Uid() == nil {
			return evalHavingTri(ctx, conn, row, fields[0].Expression())
		}
		return triFalse, api.NewErrorf(api.ErrCodeUnsupportedOperation, "HAVING does not support multi-field / named record constructor")
	}
	// HAVING-style: the full comparison is the expression atom (BinaryComparisonPredicateContext).
	compPred, ok := pred.ExpressionAtom().(*antlrgen.BinaryComparisonPredicateContext)
	if !ok {
		return triFalse, api.NewErrorf(api.ErrCodeUnsupportedOperation, "HAVING supports only comparison predicates, got %T", pred.ExpressionAtom())
	}

	var resolveAtom func(atom antlrgen.IExpressionAtomContext) (driver.Value, error)
	resolveAtom = func(atom antlrgen.IExpressionAtomContext) (driver.Value, error) {
		switch a := atom.(type) {
		case *antlrgen.ConstantExpressionAtomContext:
			return evalConstant(a.Constant())
		case *antlrgen.FullColumnNameExpressionAtomContext:
			name := functions.FullIdToName(a.FullColumnName().FullId())
			v, found := row[name]
			if !found {
				return nil, api.NewErrorf(api.ErrCodeUndefinedColumn, "HAVING column %q not in SELECT list", name)
			}
			return v, nil
		case *antlrgen.FunctionCallExpressionAtomContext:
			// Aggregate function reference — match by reconstructed output name.
			agg, aggok := a.FunctionCall().(*antlrgen.AggregateFunctionCallContext)
			if !aggok {
				return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported HAVING function call %T", a.FunctionCall())
			}
			awf, awfok := agg.AggregateWindowedFunction().(*antlrgen.AggregateWindowedFunctionContext)
			if !awfok {
				return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported HAVING aggregate %T", agg.AggregateWindowedFunction())
			}
			// Reuse extractAwfFields which already handles both plain and
			// DISTINCT forms (COUNT(*), COUNT(col), COUNT(DISTINCT col),
			// SUM/MIN/MAX/AVG with or without ALL/DISTINCT). This keeps
			// the HAVING lookup-name in sync with the SELECT-list alias
			// computed by extractAggFunc — so SELECT COUNT(DISTINCT v)
			// HAVING COUNT(DISTINCT v) > 0 finds the same aggregate.
			_, _, _, lookupName, _, fieldsOk := extractAwfFields(awf)
			if !fieldsOk {
				return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation,
					"unsupported HAVING aggregate shape")
			}
			v, found := row[lookupName]
			if !found {
				return nil, api.NewErrorf(api.ErrCodeUndefinedColumn, "HAVING aggregate %q not in SELECT list", lookupName)
			}
			return v, nil
		case *antlrgen.MathExpressionAtomContext:
			// HAVING on arithmetic over aggregates / constants, e.g.
			// `HAVING SUM(v) * 2 > 50` or `HAVING COUNT(*) + SUM(v) > 5`.
			// Recursively resolve both sides, then apply the same math
			// operator helper that the row-level evaluator uses — NULL
			// propagation comes from applyMathOp (nil-in / nil-out).
			left, lErr := resolveAtom(a.GetLeft())
			if lErr != nil {
				return nil, lErr
			}
			right, rErr := resolveAtom(a.GetRight())
			if rErr != nil {
				return nil, rErr
			}
			return functions.ApplyMathOp(left, right, classifyMathOp(a.MathOperator()))
		case *antlrgen.BitExpressionAtomContext:
			// Same shape as MathExpression but with bitwise ops. HAVING on
			// bitwise expressions (`COUNT(*) & 1`) is unusual but valid and
			// costs nothing to mirror.
			left, lErr := resolveAtom(a.GetLeft())
			if lErr != nil {
				return nil, lErr
			}
			right, rErr := resolveAtom(a.GetRight())
			if rErr != nil {
				return nil, rErr
			}
			return functions.ApplyBitOp(left, right, classifyBitOp(a.BitOperator()))
		case *antlrgen.SubqueryExpressionAtomContext:
			// HAVING `agg <op> (SELECT ... )` — uncorrelated subquery
			// pre-evaluated before the outer query started. Look up the
			// cached scalar.
			return evalScalarSubquery(ctx, conn, a.Query())
		default:
			return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported HAVING atom %T", atom)
		}
	}

	leftVal, err := resolveAtom(compPred.GetLeft())
	if err != nil {
		return triFalse, err
	}
	rightVal, err := resolveAtom(compPred.GetRight())
	if err != nil {
		return triFalse, err
	}
	opText := classifyComparisonOp(compPred.ComparisonOperator())
	switch opText {
	case "IS DISTINCT FROM":
		return triFromBool(!nullSafeEqual(leftVal, rightVal)), nil
	case "IS NOT DISTINCT FROM":
		return triFromBool(nullSafeEqual(leftVal, rightVal)), nil
	}
	if leftVal == nil || rightVal == nil {
		return triNull, nil
	}
	if !valuesComparable(leftVal, rightVal) {
		return triFalse, api.NewErrorf(api.ErrCodeDatatypeMismatch,
			"The operands of a comparison operator are not compatible.")
	}
	cmp := functions.CompareValues(leftVal, rightVal)
	switch opText {
	case "=":
		return triFromBool(cmp == 0), nil
	case "!=", "<>":
		return triFromBool(cmp != 0), nil
	case "<":
		return triFromBool(cmp < 0), nil
	case ">":
		return triFromBool(cmp > 0), nil
	case "<=":
		return triFromBool(cmp <= 0), nil
	case ">=":
		return triFromBool(cmp >= 0), nil
	}
	return triFalse, api.NewErrorf(api.ErrCodeUnsupportedOperation, "HAVING: unsupported operator %q", opText)
}

// evalPredicateOnMap evaluates a WHERE-style PredicatedExpressionContext against
// a map[string]driver.Value row. Handles IS NULL, LIKE, BETWEEN, IN, comparisons.
func evalPredicateOnMapTri(ctx context.Context, conn *EmbeddedConnection, row map[string]driver.Value, pred *antlrgen.PredicatedExpressionContext) (triBool, error) {
	fieldVal, err := evalExprAtomOnMap(ctx, conn, row, pred.ExpressionAtom())
	if err != nil {
		return triFalse, err
	}

	if pred.Predicate() == nil {
		// Leaf expression (e.g. a boolean constant) — treat NULL as UNKNOWN,
		// otherwise use truthiness.
		if fieldVal == nil {
			return triNull, nil
		}
		return triFromBool(functions.IsTruthy(fieldVal)), nil
	}

	switch p := pred.Predicate().(type) {
	case *antlrgen.IsExpressionContext:
		// IS NULL / IS TRUE / IS FALSE are always 2-state.
		negated := p.NOT() != nil
		isNull := fieldVal == nil
		switch {
		case p.NULL_LITERAL() != nil:
			res := isNull
			if negated {
				res = !res
			}
			return triFromBool(res), nil
		case p.TRUE() != nil:
			b, _ := fieldVal.(bool)
			res := b
			if negated {
				res = !res
			}
			return triFromBool(res), nil
		case p.FALSE() != nil:
			b, _ := fieldVal.(bool)
			res := !b && fieldVal != nil
			if negated {
				res = !res
			}
			return triFromBool(res), nil
		}
		return triFalse, nil

	case *antlrgen.LikePredicateContext:
		if fieldVal == nil {
			return triNull, nil
		}
		s, ok := fieldVal.(string)
		if !ok {
			// Proto path errors on non-string LIKE; match that for consistency.
			return triFalse, api.NewErrorf(api.ErrCodeInvalidParameter,
				"LIKE requires a string expression, got %T", fieldVal)
		}
		patternLit := p.GetPattern().GetText()
		var escape rune = -1
		if esc := p.GetEscape(); esc != nil {
			escStr := functions.StripStringLiteralQuotes(esc.GetText())
			runes := []rune(escStr)
			if len(runes) != 1 {
				return triFalse, api.NewErrorf(api.ErrCodeInvalidParameter,
					"LIKE ESCAPE must be exactly one character, got %q", escStr)
			}
			escape = runes[0]
		}
		matched := functions.LikeMatch(functions.StripStringLiteralQuotes(patternLit), s, escape)
		if p.NOT() != nil {
			matched = !matched
		}
		return triFromBool(matched), nil

	case *antlrgen.BetweenComparisonPredicateContext:
		// Mirror evalBetweenPredicateTri's Kleene decomposition. The
		// previous map-path version short-circuited on any-NULL bound
		// to triNull, which is wrong for NOT BETWEEN: `0 NOT BETWEEN
		// 1 AND NULL` is `(0 < 1) OR (0 > NULL)` = `(TRUE OR UNKNOWN)`
		// = TRUE, but the short-circuit returned UNKNOWN and silently
		// filtered the row out (round-5 review).
		lo, loErr := evalExprAtomOnMap(ctx, conn, row, p.GetLeft())
		if loErr != nil {
			return triFalse, loErr
		}
		hi, hiErr := evalExprAtomOnMap(ctx, conn, row, p.GetRight())
		if hiErr != nil {
			return triFalse, hiErr
		}
		// Cross-type bounds error: 42804 (DATATYPE_MISMATCH).
		if fieldVal != nil && lo != nil && !valuesComparable(fieldVal, lo) {
			return triFalse, api.NewErrorf(api.ErrCodeDatatypeMismatch,
				"The operands of a comparison operator are not compatible.")
		}
		if fieldVal != nil && hi != nil && !valuesComparable(fieldVal, hi) {
			return triFalse, api.NewErrorf(api.ErrCodeDatatypeMismatch,
				"The operands of a comparison operator are not compatible.")
		}
		compareTri := func(a, b driver.Value, want func(int) bool) triBool {
			if a == nil || b == nil {
				return triNull
			}
			return triFromBool(want(functions.CompareValues(a, b)))
		}
		if p.NOT() != nil {
			lt := compareTri(fieldVal, lo, func(c int) bool { return c < 0 })
			gt := compareTri(fieldVal, hi, func(c int) bool { return c > 0 })
			return triOr(lt, gt), nil
		}
		geLo := compareTri(fieldVal, lo, func(c int) bool { return c >= 0 })
		leHi := compareTri(fieldVal, hi, func(c int) bool { return c <= 0 })
		return triAnd(geLo, leHi), nil

	case *antlrgen.InPredicateContext:
		if fieldVal == nil {
			return triNull, nil // NULL [NOT] IN (...) = UNKNOWN
		}
		if p.InList().QueryExpressionBody() != nil {
			// Java alignment (mirror of evalInPredicateTri): IN with a
			// subquery argument is not supported — Java's
			// AstNormalizer.visitInPredicate NPEs because the visitor
			// doesn't implement the queryExpressionBody alternative.
			// Go rejects with a clean message; use EXISTS or JOIN.
			return triFalse, api.NewError(api.ErrCodeUnsupportedQuery,
				"IN with a subquery argument is not supported; use EXISTS or a JOIN")
		}
		// Same grammar-shape bail as evalInPredicateTri — `IN ?` /
		// `IN someCol` parse through the preparedStatementParameter /
		// fullColumnName alternatives, which don't carry an
		// ExpressionsContext. The previous silent-FALSE (and silent-
		// TRUE for NOT IN) behaviour was surprising; align with the
		// proto path and surface 0A000 for every non-parenthesized-
		// list, non-subquery IN.
		if p.InList().Expressions() == nil {
			return triFalse, api.NewErrorf(api.ErrCodeUnsupportedOperation,
				"IN requires a parenthesized expression list or subquery")
		}
		for _, inExpr := range p.InList().Expressions().AllExpression() {
			litVal, litErr := evalExprOnMap(ctx, conn, row, inExpr)
			if litErr != nil {
				return triFalse, litErr
			}
			if litVal == nil {
				return triFalse, api.NewErrorf(api.ErrCodeCannotConvertType,
					"NULL values are not allowed in the IN list")
			}
			if !valuesComparable(fieldVal, litVal) {
				return triFalse, api.NewErrorf(api.ErrCodeDatatypeMismatch,
					"The operands of a comparison operator are not compatible.")
			}
			if valuesEqual(fieldVal, litVal) {
				if p.NOT() != nil {
					return triFalse, nil
				}
				return triTrue, nil
			}
		}
		if p.NOT() != nil {
			return triTrue, nil
		}
		return triFalse, nil
	}

	bcp, ok := pred.ExpressionAtom().(*antlrgen.BinaryComparisonPredicateContext)
	if ok {
		rightVal, err := evalExprAtomOnMap(ctx, conn, row, bcp.GetRight())
		if err != nil {
			return triFalse, err
		}
		opText := classifyComparisonOp(bcp.ComparisonOperator())
		switch opText {
		case "IS DISTINCT FROM":
			return triFromBool(!nullSafeEqual(fieldVal, rightVal)), nil
		case "IS NOT DISTINCT FROM":
			return triFromBool(nullSafeEqual(fieldVal, rightVal)), nil
		}
		if fieldVal == nil || rightVal == nil {
			return triNull, nil
		}
		if !valuesComparable(fieldVal, rightVal) {
			return triFalse, api.NewErrorf(api.ErrCodeDatatypeMismatch,
				"The operands of a comparison operator are not compatible.")
		}
		cmp := functions.CompareValues(fieldVal, rightVal)
		switch opText {
		case "=":
			return triFromBool(cmp == 0), nil
		case "!=", "<>":
			return triFromBool(cmp != 0), nil
		case "<":
			return triFromBool(cmp < 0), nil
		case ">":
			return triFromBool(cmp > 0), nil
		case "<=":
			return triFromBool(cmp <= 0), nil
		case ">=":
			return triFromBool(cmp >= 0), nil
		default:
			return triFalse, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported comparison operator %q", opText)
		}
	}
	return triFalse, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported predicate type %T in map eval", pred.Predicate())
}

// evalPredicateOnMapExpr is the bool wrapper used by WHERE/ON/HAVING filter
// sites. The Tri variant carries the UNKNOWN flag through AND/OR/NOT; here we
// collapse it to false at the filter boundary.
func evalPredicateOnMapExpr(ctx context.Context, conn *EmbeddedConnection, row map[string]driver.Value, expr antlrgen.IExpressionContext) (bool, error) {
	t, err := evalPredicateOnMapExprTri(ctx, conn, row, expr)
	return t.IsTrue(), err
}

// evalPredicateOnMapExprTri mirrors evalExprPredicateTri but resolves column
// references from a map[string]driver.Value (used for JOIN/CTE/derived-table
// paths).
func evalPredicateOnMapExprTri(ctx context.Context, conn *EmbeddedConnection, row map[string]driver.Value, expr antlrgen.IExpressionContext) (triBool, error) {
	switch e := expr.(type) {
	case *antlrgen.ExistsExpressionAtomContext:
		if conn == nil {
			return triFalse, api.NewErrorf(api.ErrCodeUnsupportedOperation, "EXISTS subquery not supported in this context")
		}
		defer conn.pushOuterScope(outerScopeFromMapRow(row))()
		_, _, subRows, subErr := conn.execQueryBodyRows(ctx, e.Query().QueryExpressionBody())
		if subErr != nil {
			return triFalse, subErr
		}
		return triFromBool(len(subRows) > 0), nil
	case *antlrgen.LogicalExpressionContext:
		left, err := evalPredicateOnMapExprTri(ctx, conn, row, e.Expression(0))
		if err != nil {
			return triFalse, err
		}
		op := e.LogicalOperator()
		isAnd := op.AND() != nil || len(op.AllBIT_AND_OP()) >= 2
		isOr := op.OR() != nil || len(op.AllBIT_OR_OP()) >= 2
		isXor := op.XOR() != nil
		switch {
		case isAnd:
			if left == triFalse {
				return triFalse, nil
			}
			right, err := evalPredicateOnMapExprTri(ctx, conn, row, e.Expression(1))
			if err != nil {
				return triFalse, err
			}
			return triAnd(left, right), nil
		case isOr:
			if left == triTrue {
				return triTrue, nil
			}
			right, err := evalPredicateOnMapExprTri(ctx, conn, row, e.Expression(1))
			if err != nil {
				return triFalse, err
			}
			return triOr(left, right), nil
		case isXor:
			// SQL XOR: any NULL operand → NULL (can't short-circuit
			// without both concrete). Mirrors evalExprPredicateTri.
			right, err := evalPredicateOnMapExprTri(ctx, conn, row, e.Expression(1))
			if err != nil {
				return triFalse, err
			}
			if left == triNull || right == triNull {
				return triNull, nil
			}
			return triFromBool((left == triTrue) != (right == triTrue)), nil
		}
		return triFalse, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported logical operator %q", op.GetText())
	case *antlrgen.NotExpressionContext:
		v, err := evalPredicateOnMapExprTri(ctx, conn, row, e.Expression())
		if err != nil {
			return triFalse, err
		}
		return v.Not(), nil
	case *antlrgen.PredicatedExpressionContext:
		if e.Predicate() != nil {
			return evalPredicateOnMapTri(ctx, conn, row, e)
		}
		// No separate predicate — expression atom (e.g. BinaryComparisonPredicateContext).
		bcp, ok := e.ExpressionAtom().(*antlrgen.BinaryComparisonPredicateContext)
		if ok {
			left, err := evalExprAtomOnMap(ctx, conn, row, bcp.GetLeft())
			if err != nil {
				return triFalse, err
			}
			right, err := evalExprAtomOnMap(ctx, conn, row, bcp.GetRight())
			if err != nil {
				return triFalse, err
			}
			opText := classifyComparisonOp(bcp.ComparisonOperator())
			switch opText {
			case "IS DISTINCT FROM":
				return triFromBool(!nullSafeEqual(left, right)), nil
			case "IS NOT DISTINCT FROM":
				return triFromBool(nullSafeEqual(left, right)), nil
			}
			if left == nil || right == nil {
				return triNull, nil
			}
			if !valuesComparable(left, right) {
				return triFalse, api.NewErrorf(api.ErrCodeDatatypeMismatch,
					"The operands of a comparison operator are not compatible.")
			}
			cmp := functions.CompareValues(left, right)
			switch opText {
			case "=":
				return triFromBool(cmp == 0), nil
			case "!=", "<>":
				return triFromBool(cmp != 0), nil
			case "<":
				return triFromBool(cmp < 0), nil
			case ">":
				return triFromBool(cmp > 0), nil
			case "<=":
				return triFromBool(cmp <= 0), nil
			case ">=":
				return triFromBool(cmp >= 0), nil
			default:
				return triFalse, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported comparison operator %q", opText)
			}
		}
		v, err := evalExprAtomOnMap(ctx, conn, row, e.ExpressionAtom())
		if err != nil {
			return triFalse, err
		}
		if v == nil {
			return triNull, nil
		}
		return triFromBool(functions.IsTruthy(v)), nil
	default:
		return triFalse, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported WHERE expression type %T in map eval", expr)
	}
}
