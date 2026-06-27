package embedded

import (
	"context"
	"database/sql/driver"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/functions"
	antlrgen "fdb.dev/pkg/relational/core/parser/gen"
)

// Map-path expression evaluator (mirror of eval_proto.go).
//
// evalExprAtomOnMap / evalExprOnMap evaluate expressions against a
// `map[string]driver.Value` row — the path used by JOIN, CTE, and
// post-aggregate row sets where the underlying representation is a
// keyed map rather than a proto message. Routes to the same shared
// scalar-function core, predicate evaluators, and value-compare
// helpers as the proto path; the only divergence is column-resolution
// (map lookup vs proto descriptor) and aggregate-name resolution
// (post-aggregation rows have function-call names like "SUM(a)" as
// keys).
//
// Phase 1c (RFC-021) plans to merge eval_proto.go and eval_map.go
// behind a uniform `Row` interface — keeping the two paths in
// dedicated files makes the duplication visible and the eventual
// unification a clean before/after diff.

// evalExprAtomOnMap resolves an expression atom using a map[string]driver.Value
// row (used for JOIN WHERE and ON condition evaluation).
func evalExprAtomOnMap(ctx context.Context, conn *EmbeddedConnection, row map[string]driver.Value, atom antlrgen.IExpressionAtomContext) (driver.Value, error) {
	switch a := atom.(type) {
	case *antlrgen.ConstantExpressionAtomContext:
		v, err := evalConstant(a.Constant())
		if err != nil {
			return nil, err
		}
		return v, nil
	case *antlrgen.FullColumnNameExpressionAtomContext:
		name := functions.FullIdToName(a.FullColumnName().FullId())
		ref := parseColRef(name)
		v, found := row[name]
		if !found && ref.isQualified() {
			// Try unqualified: "Order.amount" → "amount". The
			// qualifier-reject and correlated outer-scope paths were
			// removed with the legacy interpreter (RFC-147): the kept
			// map-path consumers (INFORMATION_SCHEMA WHERE,
			// INSERT-VALUES folding) never set a JOIN/outer scope, so
			// those branches were unreachable dead state.
			v, found = row[ref.bare()]
		}
		if !found {
			return nil, api.NewErrorf(api.ErrCodeUndefinedColumn, "column %q not found in row", name)
		}
		if m, isAmb := v.(ambiguousColumnMarker); isAmb {
			return nil, api.NewErrorf(api.ErrCodeAmbiguousColumn,
				"column reference %q is ambiguous", m.Col)
		}
		return v, nil
	case *antlrgen.BinaryComparisonPredicateContext:
		left, err := evalExprAtomOnMap(ctx, conn, row, a.GetLeft())
		if err != nil {
			return nil, err
		}
		right, err := evalExprAtomOnMap(ctx, conn, row, a.GetRight())
		if err != nil {
			return nil, err
		}
		opText := classifyComparisonOp(a.ComparisonOperator())
		switch opText {
		case "IS DISTINCT FROM":
			return !nullSafeEqual(left, right), nil
		case "IS NOT DISTINCT FROM":
			return nullSafeEqual(left, right), nil
		}
		// Java-aligned SQL 3-valued logic: NULL comparison → UNKNOWN
		// → nil at the value evaluator (NOT false; that collapsed
		// UNKNOWN to FALSE which is wrong for SELECT projection).
		if left == nil || right == nil {
			return nil, nil
		}
		if !valuesComparable(left, right) {
			return nil, api.NewErrorf(api.ErrCodeDatatypeMismatch,
				"The operands of a comparison operator are not compatible.")
		}
		cmp := functions.CompareValues(left, right)
		switch opText {
		case "=":
			return cmp == 0, nil
		case "!=", "<>":
			return cmp != 0, nil
		case "<":
			return cmp < 0, nil
		case ">":
			return cmp > 0, nil
		case "<=":
			return cmp <= 0, nil
		case ">=":
			return cmp >= 0, nil
		}
		return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported comparison operator %q", opText)
	case *antlrgen.MathExpressionAtomContext:
		left, err := evalExprAtomOnMap(ctx, conn, row, a.GetLeft())
		if err != nil {
			return nil, err
		}
		right, err := evalExprAtomOnMap(ctx, conn, row, a.GetRight())
		if err != nil {
			return nil, err
		}
		return applyArithmeticOp(left, right, classifyMathOp(a.MathOperator()))
	case *antlrgen.BitExpressionAtomContext:
		left, err := evalExprAtomOnMap(ctx, conn, row, a.GetLeft())
		if err != nil {
			return nil, err
		}
		right, err := evalExprAtomOnMap(ctx, conn, row, a.GetRight())
		if err != nil {
			return nil, err
		}
		return functions.ApplyBitOp(left, right, classifyBitOp(a.BitOperator()))
	case *antlrgen.FunctionCallExpressionAtomContext:
		// Aggregate function calls inside a row-map expression evaluate
		// by looking up the reconstructed aggregate name in the row map.
		// This is how post-aggregation SELECT expressions like
		// `SUM(a) + SUM(b)` or `COALESCE(SUM(v), 0)` get their values:
		// the emit-time rowMap is populated with {"SUM(a)": n, "SUM(b)": m}
		// exactly as evalHavingTri's resolver expects.
		if agg, ok := a.FunctionCall().(*antlrgen.AggregateFunctionCallContext); ok {
			if awf, awfok := agg.AggregateWindowedFunction().(*antlrgen.AggregateWindowedFunctionContext); awfok {
				if _, _, _, outName, _, ok := extractAwfFields(awf); ok {
					if v, present := row[outName]; present {
						return v, nil
					}
					return nil, api.NewErrorf(api.ErrCodeInvalidParameter,
						"aggregate %q not available in this context", outName)
				}
			}
		}
		return evalScalarFunctionCallOnMap(ctx, conn, row, a.FunctionCall())
	case *antlrgen.RecordConstructorExpressionAtomContext:
		// Single-field parenthesised group — unwrap and recurse. For
		// boolean inners route through the tri-state predicate
		// evaluator so NULL comparisons encode as nil (UNKNOWN) rather
		// than collapsing to false — without this, JOIN `WHERE NOT (b
		// = NULL)` would return TRUE instead of UNKNOWN because
		// evalExprOnMap's fallback through evalExprAtomOnMap collapses
		// NULL-compared operands to false at the value-evaluator
		// boundary.
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
			if pred.Predicate() != nil || looksBoolean(pred.ExpressionAtom()) {
				t, err := evalPredicateOnMapExprTri(ctx, conn, row, inner)
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
		return evalExprOnMap(ctx, conn, row, inner)
	case *antlrgen.SubqueryExpressionAtomContext:
		// Scalar subqueries are not supported in the map-path contexts that
		// route through this evaluator (system-table WHERE filters). Severed to
		// detach the legacy embedded interpreter (RFC-145 Phase 1) — these
		// callers (INFORMATION_SCHEMA, a Go-only extension) never had a working
		// cross-engine subquery shape. The real subquery query path is Cascades.
		return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation,
			"subquery is not supported in this context")
	default:
		return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported expression atom type %T in map eval", atom)
	}
}

// evalExprOnMap evaluates a scalar IExpressionContext against a map row, returning
// a driver.Value. Handles arithmetic, column refs, constants, and nested expressions.
func evalExprOnMap(ctx context.Context, conn *EmbeddedConnection, row map[string]driver.Value, expr antlrgen.IExpressionContext) (driver.Value, error) {
	switch e := expr.(type) {
	case *antlrgen.PredicatedExpressionContext:
		if e.Predicate() != nil {
			t, err := evalPredicateOnMapTri(ctx, conn, row, e)
			if err != nil {
				return nil, err
			}
			if t == triNull {
				return nil, nil
			}
			return t == triTrue, nil
		}
		return evalExprAtomOnMap(ctx, conn, row, e.ExpressionAtom())
	case *antlrgen.LogicalExpressionContext:
		// Value-eval must preserve UNKNOWN as NULL, not collapse to
		// false. `SELECT b AND TRUE FROM x` for b=NULL should project
		// NULL, matching the proto-path fix at d0f2a3a1. Using the
		// 2-valued bool wrapper here dropped UNKNOWN → false and
		// diverged from Java.
		t, err := evalPredicateOnMapExprTri(ctx, conn, row, expr)
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
	case *antlrgen.NotExpressionContext:
		// Kleene NOT: NOT TRUE = FALSE, NOT FALSE = TRUE, NOT NULL = NULL.
		t, err := evalPredicateOnMapExprTri(ctx, conn, row, e.Expression())
		if err != nil {
			return nil, err
		}
		switch t {
		case triTrue:
			return false, nil
		case triFalse:
			return true, nil
		default:
			return nil, nil
		}
	case *antlrgen.ExistsExpressionAtomContext:
		// EXISTS is not supported in the map-path value contexts that route
		// through this evaluator (system-table WHERE filters). Severed to
		// detach the legacy embedded interpreter (RFC-145 Phase 1) — these
		// callers (INFORMATION_SCHEMA, a Go-only extension) never had a working
		// cross-engine EXISTS shape. The real EXISTS query path is Cascades.
		return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation,
			"EXISTS is not supported in this context")
	default:
		return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported expression type %T in map eval", expr)
	}
}
