package embedded

import (
	"context"
	"database/sql/driver"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/functions"
	antlrgen "fdb.dev/pkg/relational/core/parser/gen"
)

// Map-path predicate evaluators (mirror of eval_predicate.go).
//
// evalPredicateOnMap{Tri,Expr,ExprTri} are the WHERE / ON evaluators
// for map-row representations. After RFC-145 removed the legacy embedded
// interpreter their sole live consumer is the INFORMATION_SCHEMA WHERE
// filter (system_tables.go filterSysRows, a Go-only extension); the
// EXISTS / subquery arms are severed (they error cleanly — the real
// EXISTS/subquery query path is Cascades).

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
			b, ok := fieldVal.(bool)
			res := ok && b
			if negated {
				res = !res
			}
			return triFromBool(res), nil
		case p.FALSE() != nil:
			b, ok := fieldVal.(bool)
			res := ok && !b
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
		// filtered the row out.
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
		sawNull := false
		for _, inExpr := range p.InList().Expressions().AllExpression() {
			litVal, litErr := evalExprOnMap(ctx, conn, row, inExpr)
			if litErr != nil {
				return triFalse, litErr
			}
			if litVal == nil {
				sawNull = true
				continue
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
		if sawNull {
			return triNull, nil
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
		// EXISTS is not supported in the map-path WHERE contexts that route
		// through this evaluator. Severed to detach the legacy embedded
		// interpreter (RFC-145 Phase 1); the only non-island caller is the
		// INFORMATION_SCHEMA WHERE filter (system_tables.go filterSysRows, a
		// Go-only extension), which never had a working cross-engine EXISTS
		// shape. The real EXISTS query path is Cascades.
		return triFalse, api.NewErrorf(api.ErrCodeUnsupportedOperation,
			"EXISTS is not supported in this context")
	case *antlrgen.LogicalExpressionContext:
		left, err := evalPredicateOnMapExprTri(ctx, conn, row, e.Expression(0))
		if err != nil {
			return triFalse, err
		}
		op := e.LogicalOperator()
		if op == nil {
			return triFalse, api.NewErrorf(api.ErrCodeUnsupportedOperation, "missing logical operator")
		}
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
