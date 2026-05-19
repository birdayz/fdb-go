package embedded

import (
	"context"
	"database/sql/driver"
	"strings"
	"time"

	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/functions"
	antlrgen "github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser/gen"
	"google.golang.org/protobuf/proto"
)

// Scalar / specific function-call dispatch — the unified
// implementation shared by the proto and map evaluator paths.
//
// exprEvaluator + predicateEvaluator are function-pointer adapters
// that abstract over how arguments are evaluated. Both
// evalScalarFunctionCallCore and evalSpecificFunctionCore drive
// every sub-expression through these — proto and map paths supply
// their own evaluators (makeProtoExprEvaluator /
// makeMapExprEvaluator) and the cores stay path-agnostic.
//
// evalScalarFunctionCallCore is the switch over scalar function
// names that fdb-relational 4.11.1.0 has in its registry: MOD /
// COALESCE / IFNULL / GREATEST / LEAST / IF / IIF / date-part fns
// (YEAR / MONTH / DAY / HOUR / MINUTE / SECOND / DAYOFMONTH /
// DAYOFWEEK / DAYOFYEAR). Names not in this list fall through to
// the default arm and emit "Unsupported operator <name>" — byte-
// equal to Java's RelationalException for the same registry-miss.
// Phase 2 Cascades replaces this with a registry-driven dispatch.
//
// evalSpecificFunctionCore handles SpecificFunctionCall nodes
// (CASE WHEN ... END, simple CASE).
//
// statementNow / beginStatement are thin shims that forward to
// Session.StatementNow / Session.BeginStatement — kept here
// because the function-call core consumes them; will be inlined
// into the session interface as Phase 1c relocations land.

// exprEvaluator is the function-pointer adapter that abstracts over the two
// expression-evaluation contexts (proto record vs. map row). Both the scalar
// and specific function cores drive all argument evaluation through this.
type exprEvaluator func(expr antlrgen.IExpressionContext) (driver.Value, error)

// predicateEvaluator is the boolean-predicate counterpart of exprEvaluator,
// used by the searched CASE WHEN branch of evalSpecificFunctionCore.
type predicateEvaluator func(expr antlrgen.IExpressionContext) (bool, error)

// evalScalarFunctionCallCore is the unified implementation shared by
// evalScalarFunctionCall (proto path) and evalScalarFunctionCallOnMap (map
// path). The two callers differ only in how they evaluate sub-expressions;
// that variation is captured in the eval / predicateEval adapters.
func evalScalarFunctionCallCore(
	now time.Time,
	eval exprEvaluator,
	predicateEval predicateEvaluator,
	fc antlrgen.IFunctionCallContext,
) (driver.Value, error) {
	// Handle CASE expressions routed through SpecificFunctionCall.
	if sf, ok := fc.(*antlrgen.SpecificFunctionCallContext); ok {
		return evalSpecificFunctionCore(now, eval, predicateEval, sf.SpecificFunction())
	}

	var name string
	var args antlrgen.IFunctionArgsContext
	switch f := fc.(type) {
	case *antlrgen.ScalarFunctionCallContext:
		name = strings.ToUpper(f.ScalarFunctionName().GetText())
		args = f.FunctionArgs()
	case *antlrgen.UserDefinedScalarFunctionCallContext:
		name = strings.ToUpper(f.UserDefinedScalarFunctionName().GetText())
		args = f.FunctionArgs()
	case *antlrgen.AggregateFunctionCallContext:
		// Java verbatim: aggregate function in scalar (e.g. WHERE)
		// context throws IllegalStateException 'unable to eval an
		// aggregation function with eval()'.
		return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation,
			"unable to eval an aggregation function with eval()")
	default:
		return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported function call type %T", fc)
	}
	var fArgs []antlrgen.IFunctionArgContext
	if args != nil {
		fArgs = args.AllFunctionArg()
	}
	// Names intentionally NOT handled — fall through to the default
	// "Unsupported operator <name>" arm. Java's fdb-relational
	// 4.11.1.0 has no entries for these in its function registry; Go
	// matches by absence of a case here, producing the byte-equal
	// rejection. Doesn't work in Java → doesn't work in Go.
	//
	//   STRING:    UPPER, LOWER, LENGTH, CHAR_LENGTH,
	//              CHARACTER_LENGTH, LEN, OCTET_LENGTH, TRIM, LTRIM,
	//              RTRIM, CONCAT, CONCAT_WS, REPLACE, LEFT, RIGHT,
	//              POSITION, REVERSE, SUBSTRING, SUBSTR
	//   MATH:      ABS, SQRT, POWER, POW, FLOOR, CEIL, CEILING,
	//              ROUND, SIGN, PI, EXP, LN, LOG
	//   DATETIME:  NOW, CURDATE, CURTIME, SYSDATE, UTC_TIMESTAMP,
	//              UTC_DATE, UTC_TIME (function-call form). The
	//              SQL-standard form CURRENT_TIMESTAMP / CURRENT_DATE
	//              / CURRENT_TIME / LOCALTIME parses through
	//              SimpleFunctionCall and stays implemented (Java's
	//              BaseVisitor.visitSimpleFunctionCall is a broken
	//              visitChildren no-op — the Go-only working impl
	//              is a correctness improvement, not a divergence).
	//   OTHER:     NULLIF (use `CASE WHEN a = b THEN NULL ELSE a END`).
	switch name {
	case "COALESCE":
		for _, fa := range fArgs {
			v, err := eval(fa.Expression())
			if err != nil {
				return nil, err
			}
			if v != nil {
				return v, nil
			}
		}
		return nil, nil
	// IFNULL intentionally NOT handled — Java rejects with
	// "Unsupported operator IFNULL". Use COALESCE instead (which
	// IS in Java's synonym map).
	// MOD function call form intentionally NOT handled — Java's
	// synonym map binds the `%` operator to "mod" but rejects the
	// function-call form `MOD(a, b)` with "Unsupported operator MOD".
	// Use `a % b` instead.
	case "GREATEST", "LEAST":
		// Java conformance: GREATEST/LEAST return NULL if any argument
		// is NULL. VariadicFunctionValue.PhysicalOperator's per-typecode
		// lambdas (GREATEST_INT/LONG/FLOAT/DOUBLE/STRING/BOOLEAN, and
		// the LEAST_* mirror) all short-circuit `if (i == null) return null`
		// on the first NULL arg. Postgres skips NULLs; Oracle and Java
		// propagate them. Match Java.
		if len(fArgs) == 0 {
			return nil, nil
		}
		best, err := eval(fArgs[0].Expression())
		if err != nil {
			return nil, err
		}
		if best == nil {
			return nil, nil
		}
		isGreatest := name == "GREATEST"
		for _, fa := range fArgs[1:] {
			v, verr := eval(fa.Expression())
			if verr != nil {
				return nil, verr
			}
			if v == nil {
				return nil, nil
			}
			// Cross-type GREATEST/LEAST errors 42804 (DATATYPE_MISMATCH),
			// matching Java's comparison-operator type-mismatch path.
			if !valuesComparable(v, best) {
				return nil, api.NewErrorf(api.ErrCodeDatatypeMismatch,
					"The operands of a comparison operator are not compatible.")
			}
			cmp := functions.CompareValues(v, best)
			if (isGreatest && cmp > 0) || (!isGreatest && cmp < 0) {
				best = v
			}
		}
		return best, nil
	// IF / IIF intentionally NOT handled — Java rejects with
	// "Unsupported operator IF". Use `CASE WHEN cond THEN x ELSE y
	// END` (searched-CASE is implemented in both engines).
	case "YEAR", "MONTH", "DAY", "HOUR", "MINUTE", "SECOND",
		"DAYOFMONTH", "DAYOFWEEK", "DAYOFYEAR":
		// Date-part functions taking a single time.Time argument.
		// SQL standard returns an integer (1-based for month/day/dow,
		// 0-based for hour/minute/second). Mostly aligns with Go's
		// time accessors; DAYOFWEEK returns 1=Sunday..7=Saturday per
		// MySQL/Oracle (Go's Weekday is 0=Sunday..6=Saturday → +1).
		if len(fArgs) < 1 {
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "%s requires 1 argument", name)
		}
		v, err := eval(fArgs[0].Expression())
		if err != nil || v == nil {
			return nil, err
		}
		t, ok := v.(time.Time)
		if !ok {
			if s, sOK := v.(string); sOK {
				if parsed, pOK := functions.ParseTimestamp(s); pOK {
					t = parsed
					ok = true
				}
			}
		}
		if !ok {
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "%s: argument must be a date/time, got %T", name, v)
		}
		switch name {
		case "YEAR":
			return int64(t.Year()), nil
		case "MONTH":
			return int64(t.Month()), nil
		case "DAY", "DAYOFMONTH":
			return int64(t.Day()), nil
		case "HOUR":
			return int64(t.Hour()), nil
		case "MINUTE":
			return int64(t.Minute()), nil
		case "SECOND":
			return int64(t.Second()), nil
		case "DAYOFWEEK":
			// MySQL convention: Sunday=1, Saturday=7.
			return int64(t.Weekday()) + 1, nil
		case "DAYOFYEAR":
			return int64(t.YearDay()), nil
		}
		return nil, nil // unreachable
	default:
		return nil, api.NewErrorf(api.ErrCodeUndefinedFunction, "Unsupported operator %s", name)
	}
}

// makeProtoExprEvaluator builds the exprEvaluator adapter for the proto path.
// evalExpr returns (any, error); driver.Value is an alias for any so the
// conversion is a no-op except we explicitly preserve nil → nil.
func makeProtoExprEvaluator(ctx context.Context, conn *EmbeddedConnection, msg proto.Message) exprEvaluator {
	return func(e antlrgen.IExpressionContext) (driver.Value, error) {
		v, err := evalExpr(ctx, conn, msg, e)
		if err != nil {
			return nil, err
		}
		if v == nil {
			return nil, nil
		}
		return driver.Value(v), nil
	}
}

// makeMapExprEvaluator builds the exprEvaluator adapter for the map path.
func makeMapExprEvaluator(ctx context.Context, conn *EmbeddedConnection, row map[string]driver.Value) exprEvaluator {
	return func(e antlrgen.IExpressionContext) (driver.Value, error) {
		return evalExprOnMap(ctx, conn, row, e)
	}
}

func evalScalarFunctionCall(ctx context.Context, conn *EmbeddedConnection, msg proto.Message, fc antlrgen.IFunctionCallContext) (any, error) {
	eval := makeProtoExprEvaluator(ctx, conn, msg)
	// CASE WHEN's condition is value-context — Java accepts a bare
	// BOOLEAN field (`CASE WHEN flag THEN …`) and converts via
	// truthiness, matching the AND/OR/NOT operand rule. Pass
	// allowBareField=true so the bare-FieldValue check at the
	// top-level WHERE/HAVING entry doesn't fire here. (TODO #41a)
	predEval := func(e antlrgen.IExpressionContext) (bool, error) {
		t, err := evalExprPredicateTri(ctx, conn, msg, e, true /* allowBareField */)
		return t.IsTrue(), err
	}
	// fdb-relational's planner returns `RelationalException:
	// Unsupported operator <name>` from the function-registry lookup
	// when no entry matches; the default arm in
	// evalScalarFunctionCallCore emits the byte-equal message.
	return evalScalarFunctionCallCore(conn.statementNow(), eval, predEval, fc)
}

func evalScalarFunctionCallOnMap(ctx context.Context, conn *EmbeddedConnection, row map[string]driver.Value, fc antlrgen.IFunctionCallContext) (driver.Value, error) {
	eval := makeMapExprEvaluator(ctx, conn, row)
	// Same value-context relaxation as the proto path — see
	// evalScalarFunctionCall comment.
	predEval := func(e antlrgen.IExpressionContext) (bool, error) {
		t, err := evalPredicateOnMapExprTri(ctx, conn, row, e)
		return t.IsTrue(), err
	}
	return evalScalarFunctionCallCore(conn.statementNow(), eval, predEval, fc)
}

// statementNow forwards to Session.StatementNow. Retained as a
// thin shim while exec* callers still live in this file; will be
// deleted as Phase 1c moves those bodies into core/plan/physical.
func (c *EmbeddedConnection) statementNow() time.Time {
	if c == nil {
		return time.Now().UTC()
	}
	return c.sess.StatementNow()
}

// beginStatement forwards to Session.BeginStatement. Thin shim —
// see statementNow's note for removal trigger.
func (c *EmbeddedConnection) beginStatement() func() {
	return c.sess.BeginStatement()
}

// evalSpecificFunctionCore is the unified implementation shared by
// evalSpecificFunction (proto path) and evalSpecificFunctionOnMap (map path).
// Handles grammar-level SpecificFunction nodes: CASE WHEN ... END, simple CASE,
// CAST(expr AS type), and the no-argument datetime / user functions
// (CURRENT_DATE, CURRENT_TIME, CURRENT_TIMESTAMP, LOCALTIME, CURRENT_USER).
// The searched CASE branch needs a boolean predicate evaluator, hence
// predicateEval in addition to eval.
func evalSpecificFunctionCore(
	now time.Time,
	eval exprEvaluator,
	predicateEval predicateEvaluator,
	sf antlrgen.ISpecificFunctionContext,
) (driver.Value, error) {
	switch c := sf.(type) {
	case *antlrgen.SimpleFunctionCallContext:
		// CURRENT_DATE / CURRENT_TIME / CURRENT_TIMESTAMP / LOCALTIME /
		// CURRENT_USER. SQL standard says all references to these
		// functions within one statement return the same value (statement
		// timestamp). `now` is captured by the caller from
		// conn.statementNow() at the start of statement execution.
		switch {
		case c.CURRENT_DATE() != nil:
			return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC), nil
		case c.CURRENT_TIMESTAMP() != nil, c.LOCALTIME() != nil:
			return now, nil
		case c.CURRENT_TIME() != nil:
			// CURRENT_TIME returns just the time-of-day portion; we
			// surface the full timestamp because Go has no time-only
			// type and yamsql doesn't pin time-only values either.
			return now, nil
		case c.CURRENT_USER() != nil:
			// No user-identity concept yet; return empty string. The
			// connection tracks dbPath/schema, not a user. Java's
			// fdb-relational returns empty too.
			return "", nil
		}
		return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported simple function call")
	case *antlrgen.CaseFunctionCallContext:
		// Searched CASE: CASE WHEN cond THEN val ... [ELSE val] END
		// WHEN conditions are full boolean expressions (comparisons, AND/OR, etc.).
		for _, alt := range c.AllCaseFuncAlternative() {
			ok, err := predicateEval(alt.GetCondition().Expression())
			if err != nil {
				return nil, err
			}
			if ok {
				return eval(alt.GetConsequent().Expression())
			}
		}
		if c.GetElseArg() != nil {
			return eval(c.GetElseArg().Expression())
		}
		return nil, nil
	case *antlrgen.CaseExpressionFunctionCallContext:
		// Simple CASE: CASE expr WHEN val THEN result ... [ELSE result] END
		operand, err := eval(c.Expression())
		if err != nil {
			return nil, err
		}
		// SQL spec: NULL never matches in simple CASE (NULL = NULL is UNKNOWN).
		if operand != nil {
			for _, alt := range c.AllCaseFuncAlternative() {
				whenVal, err := eval(alt.GetCondition().Expression())
				if err != nil {
					return nil, err
				}
				if whenVal != nil && valuesEqual(operand, whenVal) {
					return eval(alt.GetConsequent().Expression())
				}
			}
		}
		if c.GetElseArg() != nil {
			return eval(c.GetElseArg().Expression())
		}
		return nil, nil
	case *antlrgen.DataTypeFunctionCallContext:
		// CAST(expr AS type)
		val, err := eval(c.Expression())
		if err != nil {
			return nil, err
		}
		if val == nil {
			return nil, nil // CAST(NULL AS type) = NULL
		}
		typeName := classifyPrimitiveType(c.ConvertedDataType())
		return functions.CastValue(val, typeName)
	default:
		return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported specific function %T", sf)
	}
}
