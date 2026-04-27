package embedded

import (
	"context"
	"database/sql/driver"
	"strings"

	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/functions"
	antlrgen "github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser/gen"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Proto-path predicate evaluators.
//
// evalPredicate is the WHERE-clause entry point: returns true iff
// msg satisfies whereExpr (NULL whereExpr = always true). The
// downstream tri-state evaluator (evalExprPredicateTri) handles the
// SQL Kleene 3-valued logic so NOT / AND / OR preserve UNKNOWN
// instead of collapsing it to FALSE.
//
// Per-shape predicate handlers:
//   evalComparisonPredicateTri  `=` / `<>` / `<` / `<=` / `>` / `>=`
//   evalInPredicateTri          `[NOT] IN (...)` (literal list +
//                                subquery + parameter list)
//   evalIsNullPredicate         `IS NULL` / `IS NOT NULL` (2-valued)
//   evalLikePredicateTri        `LIKE pattern [ESCAPE c]`
//   evalBetweenPredicateTri     `BETWEEN lo AND hi`
//
// The map-path mirror lives in connection.go (evalPredicateOnMap*) —
// RFC-021 Phase 1c plans to merge the two paths behind a uniform
// Row interface.

// evalPredicate returns true if msg satisfies whereExpr.
// Only col = constant comparisons are supported. If whereExpr is nil, returns true.
func evalPredicate(ctx context.Context, conn *EmbeddedConnection, msg proto.Message, whereExpr antlrgen.IWhereExprContext) (bool, error) {
	if whereExpr == nil {
		return true, nil
	}
	return evalExprPredicate(ctx, conn, msg, whereExpr.Expression())
}

// evalExprPredicate evaluates an IExpressionContext as a boolean predicate.
// Supports: col = constant, col != constant, col < constant, col > constant,
// col <= constant, col >= constant, AND, OR, NOT.
func evalExprPredicate(ctx context.Context, conn *EmbeddedConnection, msg proto.Message, expr antlrgen.IExpressionContext) (bool, error) {
	t, err := evalExprPredicateTri(ctx, conn, msg, expr)
	return t.IsTrue(), err
}

// evalExprPredicateTri is the Kleene three-valued implementation: UNKNOWN
// propagates through AND/OR/NOT so `NOT (x = NULL)` correctly stays UNKNOWN
// (filtered out) instead of flipping to TRUE. The bool wrapper above
// collapses UNKNOWN→false at the WHERE/HAVING filter boundary.
//
// Top-level WHERE / HAVING entry: a bare FieldValue (`WHERE flag`) is
// rejected to match fdb-relational's planner. Operands of AND/OR/NOT/XOR
// inside any context, and any expression in projection context, MAY be
// bare FieldValues — Java accepts those and converts via truthiness.
// See evalExprPredicateTriCtx.
func evalExprPredicateTri(ctx context.Context, conn *EmbeddedConnection, msg proto.Message, expr antlrgen.IExpressionContext) (triBool, error) {
	return evalExprPredicateTriCtx(ctx, conn, msg, expr, false /* allowBareField */)
}

// evalExprPredicateTriCtx is the param-threaded form. allowBareField=true
// permits a bare FullColumnName atom to be evaluated as a value (with
// IsTruthy → triBool); allowBareField=false rejects it the way Java's
// planner rejects `WHERE flag`.
func evalExprPredicateTriCtx(ctx context.Context, conn *EmbeddedConnection, msg proto.Message, expr antlrgen.IExpressionContext, allowBareField bool) (triBool, error) {
	switch e := expr.(type) {
	case *antlrgen.ExistsExpressionAtomContext:
		if conn == nil {
			return triFalse, api.NewErrorf(api.ErrCodeUnsupportedOperation, "EXISTS subquery not supported in this context")
		}
		// Push outer-row scope so a correlated inner reference like
		// `outer_tbl.col` resolves against this msg via resolveOuterColumn.
		// Qualifier taken from the proto descriptor name (single-source
		// FROM without an explicit AS alias — the common case).
		defer conn.pushOuterScope(outerScopeFromMsg(conn, msg))()
		_, _, subRows, subErr := conn.execQueryBodyRows(ctx, e.Query().QueryExpressionBody())
		if subErr != nil {
			return triFalse, subErr
		}
		return triFromBool(len(subRows) > 0), nil

	case *antlrgen.LogicalExpressionContext:
		// Operands of boolean operators are value-context — Java accepts
		// `b AND TRUE` / `NOT b` / `b OR FALSE` over a BOOLEAN column.
		// Pass allowBareField=true to the recursive operand evaluator.
		left, err := evalExprPredicateTriCtx(ctx, conn, msg, e.Expression(0), true)
		if err != nil {
			return triFalse, err
		}
		op := e.LogicalOperator()
		// Grammar: AND | '&' '&' | XOR | OR | '|' '|'. op.AND()/OR()/XOR()
		// are only non-nil for the keyword forms; the symbolic `&&` and
		// `||` forms need text-based detection.
		opText := strings.ReplaceAll(op.GetText(), " ", "")
		isAnd := op.AND() != nil || opText == "&&"
		isOr := op.OR() != nil || opText == "||"
		isXor := op.XOR() != nil
		switch {
		case isAnd:
			if left == triFalse {
				return triFalse, nil // short-circuit
			}
			right, err := evalExprPredicateTriCtx(ctx, conn, msg, e.Expression(1), true)
			if err != nil {
				return triFalse, err
			}
			return triAnd(left, right), nil
		case isOr:
			if left == triTrue {
				return triTrue, nil // short-circuit
			}
			right, err := evalExprPredicateTriCtx(ctx, conn, msg, e.Expression(1), true)
			if err != nil {
				return triFalse, err
			}
			return triOr(left, right), nil
		case isXor:
			// SQL XOR: a XOR b = (a AND NOT b) OR (NOT a AND b). Any NULL
			// operand → NULL (can't short-circuit without both concrete).
			right, err := evalExprPredicateTriCtx(ctx, conn, msg, e.Expression(1), true)
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
		// Operand of NOT is value-context — `NOT b` over a BOOLEAN column
		// is accepted by Java. Pass allowBareField=true.
		v, err := evalExprPredicateTriCtx(ctx, conn, msg, e.Expression(), true)
		if err != nil {
			return triFalse, err
		}
		return v.Not(), nil

	case *antlrgen.PredicatedExpressionContext:
		if e.Predicate() != nil {
			switch p := e.Predicate().(type) {
			case *antlrgen.InPredicateContext:
				return evalInPredicateTri(ctx, conn, msg, e, p)
			case *antlrgen.IsExpressionContext:
				// IS NULL / IS TRUE / IS FALSE are always 2-state (never UNKNOWN).
				b, err := evalIsNullPredicate(ctx, conn, msg, e, p)
				return triFromBool(b), err
			case *antlrgen.LikePredicateContext:
				return evalLikePredicateTri(ctx, conn, msg, e, p)
			case *antlrgen.BetweenComparisonPredicateContext:
				return evalBetweenPredicateTri(ctx, conn, msg, e, p)
			}
		}
		return evalComparisonPredicateTriCtx(ctx, conn, msg, e, allowBareField)

	default:
		return triFalse, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported WHERE expression type %T", expr)
	}
}

// evalComparisonPredicateTri handles a leaf comparison between two arbitrary
// expressions. Returns triNull when either operand is NULL so that enclosing
// NOT/AND/OR can apply proper Kleene logic (previously NULL collapsed to FALSE
// and `NOT (x = NULL)` returned TRUE).
//
// Top-level WHERE / HAVING context (allowBareField=false): rejects bare
// FullColumnName atoms to match fdb-relational ("expected BooleanValue
// but got FieldValue"). Operand-of-boolean-op or projection context
// (allowBareField=true): evaluates the column as a value and uses
// IsTruthy, since Java accepts `b AND TRUE` / `NOT b` / `SELECT b OR
// FALSE` over a BOOLEAN column.
func evalComparisonPredicateTri(ctx context.Context, conn *EmbeddedConnection, msg proto.Message, pred *antlrgen.PredicatedExpressionContext) (triBool, error) {
	return evalComparisonPredicateTriCtx(ctx, conn, msg, pred, false /* allowBareField */)
}

func evalComparisonPredicateTriCtx(ctx context.Context, conn *EmbeddedConnection, msg proto.Message, pred *antlrgen.PredicatedExpressionContext, allowBareField bool) (triBool, error) {
	bcp, ok := pred.ExpressionAtom().(*antlrgen.BinaryComparisonPredicateContext)
	if !ok {
		// Bare column reference as a top-level WHERE predicate (`WHERE
		// flag` for a BOOLEAN column) is rejected by fdb-relational
		// with "expected BooleanValue but got FieldValue". The planner
		// requires explicit comparisons (`WHERE flag = TRUE`) — a
		// FieldValue can't be implicitly coerced into a boolean
		// predicate at top level. Match that strictness here.
		//
		// When allowBareField=true (operand of AND/OR/NOT/XOR, or any
		// projection context), Java accepts the bare column and
		// converts via truthiness. Fall through to value-eval below.
		if _, isFieldValue := pred.ExpressionAtom().(*antlrgen.FullColumnNameExpressionAtomContext); isFieldValue && !allowBareField {
			return triFalse, api.NewErrorf(api.ErrCodeUnsupportedOperation,
				"expected BooleanValue but got FieldValue: bare column reference cannot be used as a predicate; use an explicit comparison (e.g. col = TRUE)")
		}
		// Non-comparison atom (e.g. `WHERE CASE WHEN ... END`, `WHERE some_bool_fn(x)`),
		// or bare FieldValue in operand/projection context.
		// Evaluate as a value. NULL result is UNKNOWN; else use truthiness.
		v, err := evalExprAtom(ctx, conn, msg, pred.ExpressionAtom())
		if err != nil {
			return triFalse, err
		}
		if v == nil {
			return triNull, nil
		}
		return triFromBool(functions.IsTruthy(v)), nil
	}
	opText := bcp.ComparisonOperator().GetText()

	left, err := evalExprAtom(ctx, conn, msg, bcp.GetLeft())
	if err != nil {
		return triFalse, err
	}
	right, err := evalExprAtom(ctx, conn, msg, bcp.GetRight())
	if err != nil {
		return triFalse, err
	}
	// SQL `IS [NOT] DISTINCT FROM` is null-safe equality — it always
	// returns TRUE or FALSE, never UNKNOWN, even when operands are NULL.
	// Grammar joins tokens without whitespace: `IS DISTINCT FROM` →
	// "ISDISTINCTFROM", `IS NOT DISTINCT FROM` → "ISNOTDISTINCTFROM".
	// Must branch BEFORE the any-NULL → UNKNOWN fallback below.
	switch opText {
	case "ISDISTINCTFROM":
		return triFromBool(!nullSafeEqual(left, right)), nil
	case "ISNOTDISTINCTFROM":
		return triFromBool(nullSafeEqual(left, right)), nil
	}
	// SQL 3-valued logic: any other comparison involving NULL is UNKNOWN.
	// Use IS NULL / IS NOT NULL for explicit NULL tests.
	if left == nil || right == nil {
		return triNull, nil
	}

	// Java alignment: Java's PromoteValue.isPromotionNeeded errors with
	// SemanticException(INCOMPATIBLE_TYPE) → SQLSTATE 22000
	// (CANNOT_CONVERT_TYPE) when the two operands have non-promotable
	// types (e.g. STRING vs BIGINT). Pre-fix Go silently returned
	// FALSE for these comparisons → empty result set, the dangerous
	// kind of bug. Now we error to match Java.
	if !valuesComparable(left, right) {
		return triFalse, api.NewErrorf(api.ErrCodeCannotConvertType,
			"cannot compare %T with %T", left, right)
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

// evalInPredicate handles: expr [NOT] IN (val1, val2, ...) or expr [NOT] IN (subquery)
func evalInPredicateTri(ctx context.Context, conn *EmbeddedConnection, msg proto.Message, pred *antlrgen.PredicatedExpressionContext, in *antlrgen.InPredicateContext) (triBool, error) {
	var fieldVal driver.Value
	if colAtom, ok := pred.ExpressionAtom().(*antlrgen.FullColumnNameExpressionAtomContext); ok {
		// Column: use proto Has() so unset optionals (SQL NULL) yield UNKNOWN.
		colName := functions.FullIdToName(colAtom.FullColumnName().FullId())
		fd := msg.ProtoReflect().Descriptor().Fields().ByName(protoreflect.Name(colName))
		if fd == nil {
			return triFalse, api.NewErrorf(api.ErrCodeUndefinedColumn, "column %q not found", colName)
		}
		if !msg.ProtoReflect().Has(fd) {
			return triNull, nil // NULL [NOT] IN (...) = UNKNOWN
		}
		fieldVal = functions.ProtoValueToDriver(fd, msg.ProtoReflect().Get(fd))
	} else {
		v, err := evalExprAtom(ctx, conn, msg, pred.ExpressionAtom())
		if err != nil {
			return triFalse, err
		}
		if v == nil {
			return triNull, nil // NULL [NOT] IN (...) = UNKNOWN
		}
		fieldVal = v
	}

	if qb := in.InList().QueryExpressionBody(); qb != nil {
		if conn == nil {
			return triFalse, api.NewErrorf(api.ErrCodeUnsupportedOperation, "subquery IN not supported in this context")
		}
		defer conn.pushOuterScope(outerScopeFromMsg(conn, msg))()
		subCols, _, subRows, err := conn.execQueryBodyRows(ctx, qb)
		if err != nil {
			return triFalse, err
		}
		// SQL standard: `x IN (SELECT a, b FROM t)` is a column-count
		// mismatch error (row constructor IN needs `(a, b) IN (...)`).
		// Previously matchSubqueryIN silently compared against column 0
		// only — wrong semantics.
		if len(subCols) != 1 {
			return triFalse, api.NewErrorf(api.ErrCodeInvalidParameter,
				"subquery for IN must return exactly one column, got %d", len(subCols))
		}
		return matchSubqueryIN(fieldVal, subRows, in.NOT() != nil)
	}

	// The inList grammar rule admits three shapes:
	//   1. '(' (queryExpressionBody | expressions) ')' — subquery or
	//      parenthesized literal list
	//   2. preparedStatementParameter — `IN ?` / `IN :name`
	//   3. fullColumnName — `IN someCol`
	// Only shape 1 carries a non-nil Expressions() child. Shapes 2
	// and 3 hit this path with Expressions() == nil — reject cleanly
	// rather than crashing on AllExpression().
	exprsCtx := in.InList().Expressions()
	if exprsCtx == nil {
		return triFalse, api.NewErrorf(api.ErrCodeUnsupportedOperation,
			"IN requires a parenthesized expression list or subquery")
	}
	exprs := exprsCtx.AllExpression()
	var hadNullElement bool
	for _, expr := range exprs {
		// Java-aligned: IN list elements are arbitrary expressions, not
		// just constants. `b IN (1+0, 3+0, 5, 7)` is valid SQL that
		// Java's in-predicate.yamsql tests directly. Use evalExpr to
		// evaluate each element against the same proto message, allowing
		// arithmetic, function calls, even subqueries.
		litVal, err := evalExpr(ctx, conn, msg, expr)
		if err != nil {
			return triFalse, err
		}
		if litVal == nil {
			// NULL in the list can never match (x = NULL is UNKNOWN), but
			// contributes UNKNOWN to the expansion if nothing else matches.
			// SQL §8.4: `x IN (..., NULL)` = UNKNOWN, `x NOT IN (..., NULL)` = UNKNOWN.
			hadNullElement = true
			continue
		}
		// Java alignment: cross-type IN element errors 22000
		// (CANNOT_CONVERT_TYPE), matching the comparison-operator path.
		if !valuesComparable(fieldVal, litVal) {
			return triFalse, api.NewErrorf(api.ErrCodeCannotConvertType,
				"cannot compare %T with %T in IN list", fieldVal, litVal)
		}
		if valuesEqual(fieldVal, litVal) {
			if in.NOT() != nil {
				return triFalse, nil
			}
			return triTrue, nil
		}
	}
	// No element matched. If any NULL literal was seen, the overall result
	// is UNKNOWN — the row filters out in WHERE but NOT of it stays UNKNOWN.
	if hadNullElement {
		return triNull, nil
	}
	if in.NOT() != nil {
		return triTrue, nil
	}
	return triFalse, nil
}

// evalIsNullPredicate handles: expr IS [NOT] NULL / IS TRUE / IS FALSE
func evalIsNullPredicate(ctx context.Context, conn *EmbeddedConnection, msg proto.Message, pred *antlrgen.PredicatedExpressionContext, is *antlrgen.IsExpressionContext) (bool, error) {
	// Evaluate the expression on the left side (may be a column, function call, etc.).
	var fieldVal driver.Value
	if colAtom, ok := pred.ExpressionAtom().(*antlrgen.FullColumnNameExpressionAtomContext); ok {
		// Column: use proto Has() to distinguish NULL (unset optional) from zero.
		colName := functions.FullIdToName(colAtom.FullColumnName().FullId())
		fd := msg.ProtoReflect().Descriptor().Fields().ByName(protoreflect.Name(colName))
		if fd == nil {
			return false, api.NewErrorf(api.ErrCodeUndefinedColumn, "column %q not found", colName)
		}
		if msg.ProtoReflect().Has(fd) {
			fieldVal = functions.ProtoValueToDriver(fd, msg.ProtoReflect().Get(fd))
		}
	} else {
		v, err := evalExprAtom(ctx, conn, msg, pred.ExpressionAtom())
		if err != nil {
			return false, err
		}
		fieldVal = v
	}
	negated := is.NOT() != nil

	switch {
	case is.NULL_LITERAL() != nil:
		isNull := fieldVal == nil
		if negated {
			return !isNull, nil
		}
		return isNull, nil
	case is.TRUE() != nil:
		b, ok := fieldVal.(bool)
		result := ok && b
		if negated {
			return !result, nil
		}
		return result, nil
	case is.FALSE() != nil:
		b, ok := fieldVal.(bool)
		result := ok && !b
		if negated {
			return !result, nil
		}
		return result, nil
	default:
		return false, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported IS test value")
	}
}

// evalLikePredicateTri handles: expr [NOT] LIKE 'pattern' [ESCAPE 'char'].
// Supports SQL wildcards: % (any sequence) and _ (any single character).
// If ESCAPE is given, the escape char preceding %, _, or itself makes the
// following char literal. Matches Java's ExpressionVisitor.visitLikePredicate
// behaviour (escape char must be exactly one char).
// Returns triNull when the expression is NULL so NOT LIKE NULL stays UNKNOWN.
func evalLikePredicateTri(ctx context.Context, conn *EmbeddedConnection, msg proto.Message, pred *antlrgen.PredicatedExpressionContext, like *antlrgen.LikePredicateContext) (triBool, error) {
	rawVal, err := evalExprAtom(ctx, conn, msg, pred.ExpressionAtom())
	if err != nil {
		return triFalse, err
	}
	if rawVal == nil {
		return triNull, nil // NULL [NOT] LIKE pattern = UNKNOWN
	}
	s, ok2 := rawVal.(string)
	if !ok2 {
		return triFalse, api.NewErrorf(api.ErrCodeUnsupportedOperation, "LIKE requires a string expression, got %T", rawVal)
	}

	// Pattern is the first STRING_LITERAL token; strip surrounding quotes.
	patternLit := like.GetPattern().GetText()
	pattern := functions.StripStringLiteralQuotes(patternLit)

	// Optional ESCAPE 'c' clause — Java asserts length==1 too.
	var escape rune = -1
	if esc := like.GetEscape(); esc != nil {
		escStr := functions.StripStringLiteralQuotes(esc.GetText())
		runes := []rune(escStr)
		if len(runes) != 1 {
			return triFalse, api.NewErrorf(api.ErrCodeInvalidParameter,
				"LIKE ESCAPE must be exactly one character, got %q", escStr)
		}
		escape = runes[0]
	}

	matched := functions.LikeMatch(pattern, s, escape)
	if like.NOT() != nil {
		return triFromBool(!matched), nil
	}
	return triFromBool(matched), nil
}

// evalBetweenPredicateTri handles: expr [NOT] BETWEEN lo AND hi (inclusive).
//
// Java conformance: rather than collapsing any NULL to UNKNOWN, decompose
// per Java's ExpressionVisitor.visitBetweenComparisonPredicate:
//
//	x BETWEEN lo AND hi    →  (lo <= x) AND (x <= hi)
//	x NOT BETWEEN lo AND hi →  (x < lo)  OR  (x > hi)
//
// then let triAnd/triOr do Kleene short-circuit. This matters when one
// side is definitively FALSE (NOT BETWEEN) or TRUE (NOT BETWEEN with
// OR short-circuit) — e.g. `5 NOT BETWEEN 1 AND NULL` evaluates to
// `5 < 1 OR 5 > NULL` = `FALSE OR UNKNOWN` = UNKNOWN (previously correct),
// but `0 NOT BETWEEN 1 AND NULL` evaluates to `0 < 1 OR 0 > NULL` =
// `TRUE OR UNKNOWN` = TRUE (previously UNKNOWN, wrongly filtered out).
func evalBetweenPredicateTri(ctx context.Context, conn *EmbeddedConnection, msg proto.Message, pred *antlrgen.PredicatedExpressionContext, bet *antlrgen.BetweenComparisonPredicateContext) (triBool, error) {
	fieldVal, err := evalExprAtom(ctx, conn, msg, pred.ExpressionAtom())
	if err != nil {
		return triFalse, err
	}
	lo, err := evalExprAtom(ctx, conn, msg, bet.GetLeft())
	if err != nil {
		return triFalse, err
	}
	hi, err := evalExprAtom(ctx, conn, msg, bet.GetRight())
	if err != nil {
		return triFalse, err
	}

	// Cross-type bounds are an error, same as plain comparison (Java's
	// between.yamsql pins XX000 for this; we use 22000 CANNOT_CONVERT_TYPE
	// matching the rest of our cross-type rejection surface).
	if fieldVal != nil && lo != nil && !valuesComparable(fieldVal, lo) {
		return triFalse, api.NewErrorf(api.ErrCodeCannotConvertType,
			"BETWEEN bounds incompatible: cannot compare %T and %T", fieldVal, lo)
	}
	if fieldVal != nil && hi != nil && !valuesComparable(fieldVal, hi) {
		return triFalse, api.NewErrorf(api.ErrCodeCannotConvertType,
			"BETWEEN bounds incompatible: cannot compare %T and %T", fieldVal, hi)
	}

	// compareTri returns TRUE/FALSE/NULL based on whether the comparison
	// can be determined; any NULL operand yields UNKNOWN.
	compareTri := func(a, b driver.Value, want func(int) bool) triBool {
		if a == nil || b == nil {
			return triNull
		}
		return triFromBool(want(functions.CompareValues(a, b)))
	}

	if bet.NOT() != nil {
		// (x < lo) OR (x > hi)
		lt := compareTri(fieldVal, lo, func(c int) bool { return c < 0 })
		gt := compareTri(fieldVal, hi, func(c int) bool { return c > 0 })
		return triOr(lt, gt), nil
	}
	// (lo <= x) AND (x <= hi)
	geLo := compareTri(fieldVal, lo, func(c int) bool { return c >= 0 })
	leHi := compareTri(fieldVal, hi, func(c int) bool { return c <= 0 })
	return triAnd(geLo, leHi), nil
}
