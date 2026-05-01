package embedded

import (
	"context"
	"database/sql/driver"
	"math"
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
// evalScalarFunctionCallCore is the ~700-line switch over scalar
// function names (UPPER / LOWER / SUBSTRING / CONCAT / TRIM /
// REPLACE / ABS / CEILING / FLOOR / ROUND / SQRT / POWER / LOG /
// EXP / MOD / COALESCE / NULLIF / CAST / CURRENT_TIMESTAMP / …).
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
//
// unsupportedFmt is the format string ("... %q ...") used for the default
// case — proto and map paths use subtly different wording which we preserve
// verbatim. It must accept exactly one %q for the function name.
func evalScalarFunctionCallCore(
	now time.Time,
	eval exprEvaluator,
	predicateEval predicateEvaluator,
	unsupportedFmt string,
	unsupportedSpecificFmt string,
	fc antlrgen.IFunctionCallContext,
) (driver.Value, error) {
	// Handle CASE expressions routed through SpecificFunctionCall.
	if sf, ok := fc.(*antlrgen.SpecificFunctionCallContext); ok {
		return evalSpecificFunctionCore(now, eval, predicateEval, unsupportedSpecificFmt, sf.SpecificFunction())
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
	case "IFNULL":
		if len(fArgs) < 2 {
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "IFNULL requires 2 arguments")
		}
		v, err := eval(fArgs[0].Expression())
		if err != nil {
			return nil, err
		}
		if v != nil {
			return v, nil
		}
		return eval(fArgs[1].Expression())
	// STRING-family scalars (UPPER / LOWER / LENGTH / CHAR_LENGTH /
	// CHARACTER_LENGTH / LEN / OCTET_LENGTH / TRIM / LTRIM / RTRIM)
	// are intentionally NOT handled — fall through to the default
	// "Unsupported operator <name>" arm. Java's fdb-relational
	// 4.11.1.0 has no entries for these in its function registry,
	// so its planner surfaces RelationalException with that exact
	// message (per swingshift-64 cross-engine probe). Same
	// architectural reason in both engines: the function registry
	// has no evaluator. Doesn't work in Java → doesn't work in Go.
	// ABS / SQRT / POWER / POW intentionally NOT handled — Java's
	// fdb-relational 4.11.1.0 ArithmeticValue registry has only
	// Add / Sub / Mul / Div / Mod / bitwise ops. Other math
	// functions fall through to "Unsupported operator <name>".
	// FLOOR / CEIL / CEILING / ROUND intentionally NOT handled —
	// Java's @AutoService(BuiltInFunction.class) ArithmeticValue
	// registry has no entries; falls through to default arm.
	case "MOD":
		if len(fArgs) < 2 {
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "MOD requires 2 arguments")
		}
		av, aerr := eval(fArgs[0].Expression())
		if aerr != nil || av == nil {
			return nil, aerr
		}
		bv, berr := eval(fArgs[1].Expression())
		if berr != nil || bv == nil {
			return nil, berr
		}
		toFloat := func(v driver.Value) (float64, bool) {
			switch n := v.(type) {
			case int64:
				return float64(n), true
			case float64:
				return n, true
			}
			return 0, false
		}
		af, aok := toFloat(av)
		bf, bok := toFloat(bv)
		if !aok || !bok {
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "MOD: arguments must be numeric")
		}
		if _, aIsInt := av.(int64); aIsInt {
			if _, bIsInt := bv.(int64); bIsInt {
				if bf == 0 {
					// Integer MOD by zero — Java throws "/ by zero".
					return nil, api.NewErrorf(api.ErrCodeDivisionByZero, "/ by zero")
				}
				return int64(af) % int64(bf), nil
			}
		}
		// Float MOD by zero returns NaN per IEEE-754; Java does not throw.
		return math.Mod(af, bf), nil
	// SIGN intentionally NOT handled — falls through to default.
	// CONCAT / CONCAT_WS intentionally NOT handled — Java's function
	// registry has no entry; falls through to "Unsupported operator
	// CONCAT". Workaround: none in fdb-relational; pin rejection.
	// NULLIF is intentionally NOT handled — falls through to the default
	// "unsupported operator" arm. Mirrors fdb-relational 4.11.1.0's
	// effective non-support: Java's function registry has no entry for
	// NULLIF, so its planner returns "Unsupported operator NULLIF"
	// (CLAUDE.md gotcha). Same architectural reason in both engines:
	// the function registry has no NULLIF evaluator. Workaround:
	// rewrite as `CASE WHEN a = b THEN NULL ELSE a END` (searched-CASE
	// is implemented in both engines). Per project conformance
	// principle: doesn't work in Java → doesn't work in Go.
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
			// Java alignment: cross-type GREATEST/LEAST errors 22000
			// (CANNOT_CONVERT_TYPE), matching the comparison-operator
			// path. Pre-fix Go silently picked one via the type-name
			// string compare in compareValues, yielding semantically
			// undefined results.
			if !valuesComparable(v, best) {
				return nil, api.NewErrorf(api.ErrCodeCannotConvertType,
					"cannot compare %T with %T in %s", v, best, name)
			}
			cmp := functions.CompareValues(v, best)
			if (isGreatest && cmp > 0) || (!isGreatest && cmp < 0) {
				best = v
			}
		}
		return best, nil
	// PI / EXP / LN / LOG intentionally NOT handled — falls through
	// to default. Java's BuiltInFunction registry has no entries for
	// transcendental math.
	// REVERSE / POSITION / LEFT / RIGHT / SUBSTRING / SUBSTR /
	// REPLACE intentionally NOT handled — Java's function registry
	// has no entry; falls through to "Unsupported operator <name>".
	case "IF", "IIF":
		// IF(cond, true_val, false_val)
		if len(fArgs) < 3 {
			return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "IF requires 3 arguments")
		}
		cond, err := eval(fArgs[0].Expression())
		if err != nil {
			return nil, err
		}
		if functions.IsTruthy(cond) {
			return eval(fArgs[1].Expression())
		}
		return eval(fArgs[2].Expression())
	// NOW / CURDATE / CURTIME / SYSDATE / UTC_TIMESTAMP / UTC_DATE /
	// UTC_TIME (MySQL-style datetime aliases) intentionally NOT
	// handled — fdb-relational 4.11.1.0's function registry has no
	// entries; falls through to "Unsupported operator <name>". The
	// SQL-standard form (CURRENT_TIMESTAMP / CURRENT_DATE /
	// CURRENT_TIME / LOCALTIME) is rejected in evalSpecificFunctionCore.
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
		return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, unsupportedFmt, name)
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
	predEval := func(e antlrgen.IExpressionContext) (bool, error) {
		return evalExprPredicate(ctx, conn, msg, e)
	}
	// Java parity: fdb-relational's planner returns `RelationalException:
	// Unsupported operator <name>` from the function-registry lookup
	// when no entry matches (CLAUDE.md gotchas: "NULLIF is not
	// registered", "Common SQL scalar functions ... are NOT
	// registered"). Match the exact phrasing so cross-engine
	// ExpectErrorContains can pin identical substrings — Go's default
	// arm now produces "Unsupported operator <name>" mirroring Java.
	return evalScalarFunctionCallCore(conn.statementNow(), eval, predEval, "Unsupported operator %s", "unsupported specific function %T", fc)
}

func evalScalarFunctionCallOnMap(ctx context.Context, conn *EmbeddedConnection, row map[string]driver.Value, fc antlrgen.IFunctionCallContext) (driver.Value, error) {
	eval := makeMapExprEvaluator(ctx, conn, row)
	predEval := func(e antlrgen.IExpressionContext) (bool, error) {
		return evalPredicateOnMapExpr(ctx, conn, row, e)
	}
	return evalScalarFunctionCallCore(conn.statementNow(), eval, predEval, "Unsupported operator %s", "unsupported specific function %T", fc)
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
//
// unsupportedFmt must accept exactly one %T for the specific-function type.
func evalSpecificFunctionCore(
	now time.Time,
	eval exprEvaluator,
	predicateEval predicateEvaluator,
	unsupportedFmt string,
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
	// Simple-CASE form (`CASE expr WHEN val THEN ...`) is intentionally
	// NOT handled — falls through to the `default:` arm below
	// (ErrCodeUnsupportedOperation). Mirrors fdb-relational 4.11.1.0's
	// `BaseVisitor.visitCaseExpressionFunctionCall = visitChildren(ctx)`
	// — the simple-CASE visitor is a structural no-op there, producing
	// silently-wrong results in Java (typically returns the ELSE branch
	// regardless of subject). Same architectural reason in both engines:
	// the simple-CASE evaluator is not implemented. Searched-CASE
	// (`CASE WHEN cond THEN ...`) is implemented above and works
	// correctly. Per CLAUDE.md "Java↔Go conformance gotchas" §
	// "Parser bugs": doesn't work in Java → doesn't work in Go.
	case *antlrgen.DataTypeFunctionCallContext:
		// CAST(expr AS type)
		val, err := eval(c.Expression())
		if err != nil {
			return nil, err
		}
		if val == nil {
			return nil, nil // CAST(NULL AS type) = NULL
		}
		typeName := strings.ToUpper(c.ConvertedDataType().GetText())
		return functions.CastValue(val, typeName)
	default:
		return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, unsupportedFmt, sf)
	}
}
