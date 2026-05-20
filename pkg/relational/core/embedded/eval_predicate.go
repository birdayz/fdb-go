package embedded

import (
	"context"
	"database/sql/driver"

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
//
// Callers MUST invoke rejectTopLevelParenthesizedWhere on the WHERE
// expression once before the row-loop — this function runs per row
// and the structural check is row-independent. See
// select_query_full.go's scan loops for the hoisted call sites.
func evalPredicate(ctx context.Context, conn *EmbeddedConnection, msg proto.Message, whereExpr antlrgen.IWhereExprContext) (bool, error) {
	if whereExpr == nil {
		return true, nil
	}
	return evalExprPredicate(ctx, conn, msg, whereExpr.Expression())
}

// rejectTopLevelParenthesizedWhere mirrors fdb-relational 4.11.1.0's
// type check on the WHERE expression's underlying value. Java parses
// `WHERE (boolean_expr)` as a recordConstructor (single-element
// tuple); Expression.toUnderlyingPredicate's
// `Assert.castUnchecked(..., BooleanValue.class)` then fails with
// the verbatim "expected BooleanValue but got RecordConstructorValue".
// The check applies only to the TOP-LEVEL expression — Java accepts
// `(a) AND (b)` because the LogicalExpression's underlying value is
// a BooleanValue at the surface, even though the leaves are
// RecordConstructorValues. Match that surface check here.
//
// Hoist this once per statement, before the row-scan loop — the check
// is purely structural over the parse tree (no row dependency), so
// invoking it inside the per-row evalPredicate would re-walk the same
// AST N times for an N-row scan.
func rejectTopLevelParenthesizedWhere(expr antlrgen.IExpressionContext) error {
	pred, ok := expr.(*antlrgen.PredicatedExpressionContext)
	if !ok || pred.Predicate() != nil {
		return nil
	}
	if _, isRC := pred.ExpressionAtom().(*antlrgen.RecordConstructorExpressionAtomContext); isRC {
		return api.NewError(api.ErrCodeInvalidParameter,
			"expected BooleanValue but got RecordConstructorValue")
	}
	// Java alignment (TODO #41b): WHERE on a CASE expression that
	// returns a boolean (`WHERE CASE WHEN cond THEN TRUE … END`)
	// is rejected by fdb-relational with `expected BooleanValue but
	// got PickValue` — the planner's top-level Assert.castUnchecked
	// to BooleanValue rejects PickValue (the IR node for CASE) the
	// same way it rejects RecordConstructorValue. Match byte-equal.
	if fc, isFC := pred.ExpressionAtom().(*antlrgen.FunctionCallExpressionAtomContext); isFC {
		if sf, isSpec := fc.FunctionCall().(*antlrgen.SpecificFunctionCallContext); isSpec {
			if _, isCase := sf.SpecificFunction().(*antlrgen.CaseFunctionCallContext); isCase {
				return api.NewError(api.ErrCodeInvalidParameter,
					"expected BooleanValue but got PickValue")
			}
		}
	}
	return nil
}

// evalExprPredicate evaluates an IExpressionContext as a boolean predicate.
// Supports: col = constant, col != constant, col < constant, col > constant,
// col <= constant, col >= constant, AND, OR, NOT.
func evalExprPredicate(ctx context.Context, conn *EmbeddedConnection, msg proto.Message, expr antlrgen.IExpressionContext) (bool, error) {
	t, err := evalExprPredicateTri(ctx, conn, msg, expr, false /* allowBareField */)
	return t.IsTrue(), err
}

// evalExprPredicateTri is the Kleene three-valued implementation: UNKNOWN
// propagates through AND/OR/NOT so `NOT (x = NULL)` correctly stays UNKNOWN
// (filtered out) instead of flipping to TRUE. The bool wrapper above
// collapses UNKNOWN→false at the WHERE/HAVING filter boundary.
//
// allowBareField=true permits a bare FullColumnName atom to be evaluated as
// a value (with IsTruthy → triBool); allowBareField=false rejects it the way
// Java's planner rejects `WHERE flag`. Top-level WHERE / HAVING callers
// pass false; projection-level `evalExpr` passes true; recursive calls inside
// LogicalExpression / NotExpression / XOR always pass true (operands of
// boolean ops are value-context regardless of where the enclosing
// expression sits).
func evalExprPredicateTri(ctx context.Context, conn *EmbeddedConnection, msg proto.Message, expr antlrgen.IExpressionContext, allowBareField bool) (triBool, error) {
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
		left, err := evalExprPredicateTri(ctx, conn, msg, e.Expression(0), true)
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
				return triFalse, nil // short-circuit
			}
			right, err := evalExprPredicateTri(ctx, conn, msg, e.Expression(1), true)
			if err != nil {
				return triFalse, err
			}
			return triAnd(left, right), nil
		case isOr:
			if left == triTrue {
				return triTrue, nil // short-circuit
			}
			right, err := evalExprPredicateTri(ctx, conn, msg, e.Expression(1), true)
			if err != nil {
				return triFalse, err
			}
			return triOr(left, right), nil
		case isXor:
			// SQL XOR: a XOR b = (a AND NOT b) OR (NOT a AND b). Any NULL
			// operand → NULL (can't short-circuit without both concrete).
			right, err := evalExprPredicateTri(ctx, conn, msg, e.Expression(1), true)
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
		v, err := evalExprPredicateTri(ctx, conn, msg, e.Expression(), true)
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
		return evalComparisonPredicateTri(ctx, conn, msg, e, allowBareField)

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
func evalComparisonPredicateTri(ctx context.Context, conn *EmbeddedConnection, msg proto.Message, pred *antlrgen.PredicatedExpressionContext, allowBareField bool) (triBool, error) {
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
			// Java verbatim: "expected BooleanValue but got FieldValue".
			// Cross-engine corpus `bare_bool_where_rejected` pins
			// byte-equality.
			return triFalse, api.NewErrorf(api.ErrCodeUnsupportedOperation,
				"expected BooleanValue but got FieldValue")
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
	opText := classifyComparisonOp(bcp.ComparisonOperator())

	left, err := evalExprAtom(ctx, conn, msg, bcp.GetLeft())
	if err != nil {
		return triFalse, err
	}
	right, err := evalExprAtom(ctx, conn, msg, bcp.GetRight())
	if err != nil {
		return triFalse, err
	}
	// IS [NOT] DISTINCT FROM is null-safe — always 2-valued; branch before null-guard.
	switch opText {
	case "IS DISTINCT FROM":
		return triFromBool(!nullSafeEqual(left, right)), nil
	case "IS NOT DISTINCT FROM":
		return triFromBool(nullSafeEqual(left, right)), nil
	}
	// SQL 3-valued logic: any comparison involving NULL → UNKNOWN.
	if left == nil || right == nil {
		return triNull, nil
	}

	// Java's PromoteValue.isPromotionNeeded → SQLSTATE 42804.
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

// evalInPredicate handles: expr [NOT] IN (val1, val2, ...) or expr [NOT] IN (subquery)
func evalInPredicateTri(ctx context.Context, conn *EmbeddedConnection, msg proto.Message, pred *antlrgen.PredicatedExpressionContext, in *antlrgen.InPredicateContext) (triBool, error) {
	var fieldVal driver.Value
	if colAtom, ok := pred.ExpressionAtom().(*antlrgen.FullColumnNameExpressionAtomContext); ok {
		// Column: use proto Has() so unset optionals (SQL NULL) yield UNKNOWN.
		colName := functions.FullIdToName(colAtom.FullColumnName().FullId())
		bare := parseColRef(colName).bare()
		fd := msg.ProtoReflect().Descriptor().Fields().ByName(protoreflect.Name(bare))
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

	if in.InList().QueryExpressionBody() != nil {
		// Java alignment (architectural): fdb-relational 4.11.1.0's
		// AstNormalizer.visitInPredicate (line 437) calls
		// ParseHelpers.isConstant(ctx.inList().expressions()), but
		// `ctx.inList().expressions()` is null when the inList went
		// through the `queryExpressionBody` grammar alternative
		// (`'(' (queryExpressionBody | expressions) ')'`). The
		// visitor doesn't handle the subquery alternative —
		// ParseHelpers.isConstant has @Nonnull on its parameter and
		// dereferences ctx.expression() unconditionally → NPE. The
		// NPE is a downstream observable of "visitor doesn't
		// implement"; per CLAUDE.md principle #10 (emergent behaviour
		// over special-case checks), Go aligns with the architectural
		// reality — IN-subquery isn't supported — but emits a clean
		// Go error instead of reproducing Java's NPE. EXISTS subquery
		// and JOIN both work cleanly in both engines and are the
		// supported rewrites.
		return triFalse, api.NewError(api.ErrCodeUnsupportedQuery,
			"IN with a subquery argument is not supported; use EXISTS or a JOIN")
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
	for _, expr := range exprs {
		litVal, err := evalExpr(ctx, conn, msg, expr)
		if err != nil {
			return triFalse, err
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
			if in.NOT() != nil {
				return triFalse, nil
			}
			return triTrue, nil
		}
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
		bare := parseColRef(colName).bare()
		fd := msg.ProtoReflect().Descriptor().Fields().ByName(protoreflect.Name(bare))
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
		return triFalse, api.NewErrorf(api.ErrCodeInvalidParameter, "LIKE requires a string expression, got %T", rawVal)
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

	// Cross-type bounds are an error. Java's between.yamsql uses 42804
	// (DATATYPE_MISMATCH) for type-incompatible BETWEEN operands.
	if fieldVal != nil && lo != nil && !valuesComparable(fieldVal, lo) {
		return triFalse, api.NewErrorf(api.ErrCodeDatatypeMismatch,
			"The operands of a comparison operator are not compatible.")
	}
	if fieldVal != nil && hi != nil && !valuesComparable(fieldVal, hi) {
		return triFalse, api.NewErrorf(api.ErrCodeDatatypeMismatch,
			"The operands of a comparison operator are not compatible.")
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
