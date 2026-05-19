package embedded

import (
	"context"
	"strings"

	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/functions"
	antlrgen "github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser/gen"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Proto-path expression evaluator.
//
// evalExpr / looksBoolean / evalExprAtom evaluate a parse-tree
// expression against a proto.Message ("inner" row) — the path used
// by the single-table SELECT, UPDATE / DELETE filtering, and any
// place a record-type-keyed scan yields proto records. Mirrors the
// map-path evaluator in connection.go (evalExprAtomOnMap /
// evalExprOnMap) which targets `map[col]driver.Value` rows from
// JOIN / CTE / aggregate paths. Phase 1c (RFC-021) plans to merge
// the two paths behind a uniform `Row` interface — until then,
// keeping them in their own files makes the duplication visible
// and easier to diff.
//
// Routing rules baked in here:
//   - Boolean atoms (comparisons, parenthesised predicates) get
//     routed to evalExprPredicateTri so SQL UNKNOWN propagates as
//     nil at projection (Java parity — UNKNOWN ≠ FALSE for value
//     contexts).
//   - Single-element record constructors collapse to the inner
//     expression; multi-field constructors error.
//   - IS [NOT] DISTINCT FROM short-circuits NULL-safe equality
//     before the generic NULL → UNKNOWN check.
//   - Subqueries route through evalScalarSubquery (cache lookup +
//     SQL §9.3 cardinality validation).

// evalExpr evaluates an expression against msg, returning a scalar driver.Value.
// Used in SELECT projections, UPDATE SET, and WHERE/HAVING predicates.
// Supports: literals, column references, and binary arithmetic (+, -, *, /).
func evalExpr(ctx context.Context, conn *EmbeddedConnection, msg proto.Message, expr antlrgen.IExpressionContext) (any, error) {
	pred, ok := expr.(*antlrgen.PredicatedExpressionContext)
	if !ok {
		// Boolean expressions (AND/OR/NOT, comparisons) return bool or
		// nil-for-UNKNOWN when used as a value. Java-aligned: SELECT
		// projection of a boolean expression preserves UNKNOWN as NULL,
		// not collapses to FALSE. Use the tri-state evaluator and map
		// triNull → nil.
		//
		// Projection context: bare bool column operands of boolean ops
		// (`SELECT b AND TRUE`, `SELECT NOT b`) are accepted by Java
		// and the planner converts via truthiness — pass
		// allowBareField=true so the FullColumnName check inside
		// nested PredicatedExpression operands falls through to
		// value-eval instead of rejecting.
		t, err := evalExprPredicateTri(ctx, conn, msg, expr, true /* allowBareField */)
		if err != nil {
			return nil, err
		}
		switch t {
		case triTrue:
			return true, nil
		case triFalse:
			return false, nil
		default:
			return nil, nil
		}
	}
	// If a predicate modifier is present (IN, IS, LIKE, BETWEEN), evaluate
	// via evalExprPredicateTri so UNKNOWN propagates to NULL at projection.
	// Note: IS predicates (IS TRUE / IS FALSE / IS NULL) are 2-valued by
	// definition — the tri-state evaluator already returns triFromBool for
	// them, so their projection collapses cleanly to true/false.
	if pred.Predicate() != nil {
		t, err := evalExprPredicateTri(ctx, conn, msg, expr, false /* allowBareField */)
		if err != nil {
			return nil, err
		}
		switch t {
		case triTrue:
			return true, nil
		case triFalse:
			return false, nil
		default:
			return nil, nil
		}
	}
	return evalExprAtom(ctx, conn, msg, pred.ExpressionAtom())
}

// looksBoolean reports whether an expression atom is clearly a boolean
// (comparison or nested parenthesised boolean). Used to route a
// parenthesised group through the tri-state predicate evaluator
// instead of the value evaluator when the inner looks predicate-ish.
// False negatives are OK — they just fall through to the value path
// which handles non-boolean atoms correctly.
func looksBoolean(atom antlrgen.IExpressionAtomContext) bool {
	switch atom.(type) {
	case *antlrgen.BinaryComparisonPredicateContext:
		return true
	case *antlrgen.RecordConstructorExpressionAtomContext:
		return true
	}
	return false
}

func evalExprAtom(ctx context.Context, conn *EmbeddedConnection, msg proto.Message, atom antlrgen.IExpressionAtomContext) (any, error) {
	switch a := atom.(type) {
	case *antlrgen.ConstantExpressionAtomContext:
		return evalConstant(a.Constant())
	case *antlrgen.FullColumnNameExpressionAtomContext:
		colName := functions.FullIdToName(a.FullColumnName().FullId())
		// Try inner scope first: strip any qualifier and look up on msg.
		// For qualified `qual.col`, fall through to outer scopes when qual
		// does not match the inner msg's descriptor name — otherwise
		// `emp.id` in an inner `FROM project` would silently resolve to
		// `project.id`. Unqualified `col` prefers inner; falls through
		// only on miss.
		ref := parseColRef(colName)
		bare := ref.bare()
		qual := strings.ToUpper(ref.table)
		if msg != nil {
			// Inner qualifier match: accept the descriptor name always;
			// also accept any SQL-level alias declared by the current
			// scan (conn.currentSourceAliases, populated by scan loops
			// when they enter), so `FROM project AS p WHERE p.emp_id`
			// resolves p → project even though the descriptor is
			// PROJECT. nil conn (unit-test eval) falls back to the
			// descriptor-only check.
			innerName := strings.ToUpper(string(msg.ProtoReflect().Descriptor().Name()))
			innerMatches := qual == "" || qual == innerName
			if !innerMatches && conn != nil && conn.currentSourceAliases[qual] {
				innerMatches = true
			}
			if innerMatches {
				fd := msg.ProtoReflect().Descriptor().Fields().ByName(protoreflect.Name(bare))
				if fd != nil {
					// Absent proto2 optional fields are SQL NULL — distinct from the zero
					// value. Predicates already use Has(); function arguments must too,
					// otherwise UPPER(NULL) would produce "" instead of NULL.
					if !msg.ProtoReflect().Has(fd) {
						return nil, nil
					}
					return functions.ProtoValueToDriver(fd, msg.ProtoReflect().Get(fd)), nil
				}
			}
		}
		// Correlated subquery fallback: walk outer-row stack when inner
		// lookup failed (qualifier mismatch or missing field).
		if conn != nil && len(conn.outerScopes) > 0 {
			v, found, oerr := conn.resolveOuterColumn(colName)
			if oerr != nil {
				return nil, oerr
			}
			if found {
				return v, nil
			}
		}
		if msg == nil {
			return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "column reference %q not allowed in this context", colName)
		}
		return nil, api.NewErrorf(api.ErrCodeUndefinedColumn, "column %q not found", colName)
	case *antlrgen.MathExpressionAtomContext:
		left, err := evalExprAtom(ctx, conn, msg, a.GetLeft())
		if err != nil {
			return nil, err
		}
		right, err := evalExprAtom(ctx, conn, msg, a.GetRight())
		if err != nil {
			return nil, err
		}
		return functions.ApplyMathOp(left, right, classifyMathOp(a.MathOperator()))
	case *antlrgen.BitExpressionAtomContext:
		// Grammar: bitOperator : '<' '<' | '>' '>' | '&' | '^' | '|'
		// Java registers bitand/bitor/bitxor + shifts in SqlFunctionCatalog.
		left, err := evalExprAtom(ctx, conn, msg, a.GetLeft())
		if err != nil {
			return nil, err
		}
		right, err := evalExprAtom(ctx, conn, msg, a.GetRight())
		if err != nil {
			return nil, err
		}
		return functions.ApplyBitOp(left, right, classifyBitOp(a.BitOperator()))
	case *antlrgen.FunctionCallExpressionAtomContext:
		return evalScalarFunctionCall(ctx, conn, msg, a.FunctionCall())
	case *antlrgen.RecordConstructorExpressionAtomContext:
		// A single-field parenthesised group `(expr)` parses as a
		// RecordConstructor with one unnamed expression. SQL convention
		// is that single-element tuples are just the element — treat
		// it as the inner expression. Real multi-field record
		// constructors `(a, b)` / `(a AS x, b AS y)` still error.
		//
		// For boolean predicates like `(b = NULL)`, route through the
		// tri-state predicate evaluator so UNKNOWN propagates as nil
		// (the value-encoding of UNKNOWN — the caller in
		// evalComparisonPredicateTri maps `nil` back to triNull).
		// Without this, a NULL comparison would collapse to FALSE
		// inside the value evaluator and NOT (b = NULL) would wrongly
		// flip to TRUE.
		rc := a.RecordConstructor()
		if rc == nil {
			return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "empty record constructor")
		}
		if rc.STAR() != nil || rc.OfTypeClause() != nil {
			return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "record constructor with STAR / OF TYPE not supported")
		}
		fields := rc.AllExpressionWithOptionalName()
		if len(fields) != 1 {
			return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "multi-field record constructor not supported in this context")
		}
		f := fields[0]
		if f.AS() != nil || f.Uid() != nil {
			return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "named record field not supported in this context")
		}
		inner := f.Expression()
		if pred, ok := inner.(*antlrgen.PredicatedExpressionContext); ok {
			// If the inner expression is a bare predicate (comparison,
			// IS, LIKE, IN, BETWEEN, logical op), evaluate as tri-state.
			// Value-returning atoms fall through to evalExpr below.
			if pred.Predicate() != nil || looksBoolean(pred.ExpressionAtom()) {
				t, err := evalExprPredicateTri(ctx, conn, msg, inner, false /* allowBareField */)
				if err != nil {
					return nil, err
				}
				switch t {
				case triTrue:
					return true, nil
				case triFalse:
					return false, nil
				default:
					return nil, nil
				}
			}
		}
		// Non-predicate (e.g. arithmetic, function call, constant) —
		// evaluate as a plain value.
		return evalExpr(ctx, conn, msg, inner)
	case *antlrgen.BinaryComparisonPredicateContext:
		// Comparison used as a value (e.g. SELECT b = true, IF(a > b, ...),
		// CASE WHEN ... END). Java-aligned SQL 3-valued logic: when an
		// operand is NULL the result is UNKNOWN, encoded as nil for the
		// value evaluator. Pre-fix returned false which collapsed UNKNOWN
		// to FALSE — wrong at projection (Java returns NULL).
		left, err := evalExprAtom(ctx, conn, msg, a.GetLeft())
		if err != nil {
			return nil, err
		}
		right, err := evalExprAtom(ctx, conn, msg, a.GetRight())
		if err != nil {
			return nil, err
		}
		op := classifyComparisonOp(a.ComparisonOperator())
		switch op {
		case "IS DISTINCT FROM":
			return !nullSafeEqual(left, right), nil
		case "IS NOT DISTINCT FROM":
			return nullSafeEqual(left, right), nil
		}
		if left == nil || right == nil {
			return nil, nil
		}
		if !valuesComparable(left, right) {
			return nil, api.NewErrorf(api.ErrCodeCannotConvertType,
				"cannot compare %T with %T", left, right)
		}
		cmp := functions.CompareValues(left, right)
		switch op {
		case "=":
			return cmp == 0, nil
		case "!=", "<>":
			return cmp != 0, nil
		case "<":
			return cmp < 0, nil
		case "<=":
			return cmp <= 0, nil
		case ">":
			return cmp > 0, nil
		case ">=":
			return cmp >= 0, nil
		}
		return false, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported comparison operator %q", op)
	case *antlrgen.SubqueryExpressionAtomContext:
		return evalScalarSubquery(ctx, conn, a.Query())
	default:
		return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported expression atom %T", atom)
	}
}
