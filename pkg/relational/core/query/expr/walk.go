package expr

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	antlrgen "github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/query/semantic"
)

// WalkExpression is the parse-tree → values.Value entry point.
// For expressions that are semantically boolean predicates (bare
// column comparisons, AND/OR/NOT), use WalkPredicate instead —
// WalkExpression returns a Value, not a QueryPredicate.
//
// Dispatches by concrete ANTLR context type:
//
//   - PredicatedExpression wrapping an ExpressionAtom → walkAtom.
//   - Anything with a grammar Predicate attached (BETWEEN, IN, LIKE,
//     IS NULL) — those are predicates, not values; rejected here.
//
// walkAtom handles:
//
//   - FullColumnName → FieldValue (via ResolveIdentifier).
//   - Constant (integer / float / string / NULL / boolean) →
//     ConstantValue / NullValue / BooleanValue.
//   - MathExpression (+, -, *, /, %, MOD, DIV) → ArithmeticValue.
//   - RecordConstructor (1-element unnamed, i.e. `(x)`) → unwrap.
//   - FunctionCall:
//     · aggregate forms (COUNT/SUM/MIN/MAX/AVG) → AggregateValue;
//     · CAST/CONVERT (SpecificFunction) → CastValue;
//     · scalar UPPER/LOWER/LENGTH family → ScalarFunctionValue.
//   - PreparedStatementParameter (`?` / `?name`) → ParameterValue.
//
// Everything else returns UnsupportedExpressionShapeError so the
// caller can fall back to the existing logical-builder path.
func (r *Resolver) WalkExpression(ctx antlrgen.IExpressionContext) (values.Value, error) {
	return r.walkExpressionInner(ctx, false)
}

// WalkExpressionForProjection is like WalkExpression but also handles
// BinaryComparisonPredicate as ExpressionAtom — comparison expressions
// like `a = b`, `x IS DISTINCT FROM NULL` that appear in SELECT lists.
// Separated from WalkExpression so that CASE WHEN branches don't gain
// comparison handling (Java rejects `WHERE CASE WHEN ... THEN a < b`).
func (r *Resolver) WalkExpressionForProjection(ctx antlrgen.IExpressionContext) (values.Value, error) {
	return r.walkExpressionInner(ctx, true)
}

func (r *Resolver) walkExpressionInner(ctx antlrgen.IExpressionContext, allowComparisons bool) (values.Value, error) {
	if ctx == nil {
		return nil, fmt.Errorf("expr.WalkExpression: nil context")
	}
	switch c := ctx.(type) {
	case *antlrgen.PredicatedExpressionContext:
		if c.Predicate() != nil {
			pred, err := r.walkPredicatedExpression(c)
			if err != nil {
				return nil, err
			}
			return &predicateValue{pred: pred}, nil
		}
		if allowComparisons {
			if bc, ok := c.ExpressionAtom().(*antlrgen.BinaryComparisonPredicateContext); ok {
				pred, err := r.walkBinaryComparison(bc)
				if err != nil {
					return nil, err
				}
				return &predicateValue{pred: pred}, nil
			}
		}
		return r.walkAtom(c.ExpressionAtom())
	case *antlrgen.LogicalExpressionContext:
		pred, err := r.walkLogicalExpression(c)
		if err != nil {
			return nil, err
		}
		return &predicateValue{pred: pred}, nil
	case *antlrgen.NotExpressionContext:
		child, err := r.WalkPredicate(c.Expression())
		if err != nil {
			return nil, err
		}
		return &predicateValue{pred: r.ResolveNot(child)}, nil
	case *antlrgen.ExistsExpressionAtomContext:
		pred, err := r.walkExistsPredicate(c)
		if err != nil {
			return nil, err
		}
		return &predicateValue{pred: pred}, nil
	}
	return nil, &UnsupportedExpressionShapeError{Shape: fmt.Sprintf("%T", ctx)}
}

// walkAtom dispatches concrete ExpressionAtom variants. Returns a
// Value OR — for BinaryComparisonPredicate atoms — a
// *predicates.ComparisonPredicate wrapped as a Value, since the
// grammar treats binary comparisons as atoms but the analyzer
// surfaces them as predicates. Callers should type-switch the
// return to pick up both shapes.
func (r *Resolver) walkAtom(atom antlrgen.IExpressionAtomContext) (values.Value, error) {
	if atom == nil {
		return nil, fmt.Errorf("expr.walkAtom: nil atom")
	}
	switch a := atom.(type) {
	case *antlrgen.FullColumnNameExpressionAtomContext:
		return r.walkColumnRef(a.FullColumnName().FullId())
	case *antlrgen.ConstantExpressionAtomContext:
		return r.walkConstant(a.Constant())
	case *antlrgen.RecordConstructorExpressionAtomContext:
		// A parenthesised single expression `(x)` surfaces as a
		// RecordConstructor with exactly one child. Unwrap and
		// recurse. Multi-element or named-field record constructors
		// need dedicated support (RecordConstructorValue in cascades)
		// and aren't wired yet.
		return r.walkRecordConstructor(a.RecordConstructor())
	case *antlrgen.MathExpressionAtomContext:
		// `a + b`, `a * b`, etc. Recurse on both operands and
		// resolve via ResolveArithmetic. MOD / DIV / MODULE +
		// integer div are not wired yet — values.ArithmeticOp
		// doesn't expose them.
		return r.walkMathExpression(a)
	case *antlrgen.FunctionCallExpressionAtomContext:
		// Function call — aggregates (COUNT/SUM/MIN/MAX/AVG) +
		// CAST/CONVERT (DataTypeFunctionCall) + the seed scalar set
		// (UPPER/LOWER/LENGTH family). Names outside the seed
		// catalogue decline with UnsupportedExpressionShapeError so
		// the logical-builder text fallback catches them; the full
		// registry lands with the function-catalog port.
		return r.walkFunctionCall(a.FunctionCall())
	case *antlrgen.PreparedStatementParameterAtomContext:
		// `?` (positional) or `?name` / `$name` (named, per the
		// grammar's NAMED_PARAMETER rule [?$][A-Za-z]…) prepared-
		// statement parameter. Surfaces as a values.ParameterValue
		// — Operand composes through the comparison resolver,
		// IsConstantValue declines, ExplainValue renders `?N` /
		// `?name` for plan-cache keying.
		return r.walkPreparedParameter(a.PreparedStatementParameter())
	case *antlrgen.BitExpressionAtomContext:
		return r.walkBitExpression(a)
	case *antlrgen.SubqueryExpressionAtomContext:
		return r.walkScalarSubquery(a)
	}
	return nil, &UnsupportedExpressionShapeError{Shape: fmt.Sprintf("%T", atom)}
}

// walkPreparedParameter handles the `?` / `?name` (or `$name`)
// placeholder that appears as a PreparedStatementParameterAtom. The
// grammar's PreparedStatementParameter rule accepts either QUESTION
// (positional, rendered as a single `?`) or NAMED_PARAMETER (the
// `[?$][A-Za-z][A-Za-z0-9_/]*` token form, e.g. `?foo` / `$foo`).
//
// Positional parameters fold to the ordinal of this `?` within the
// statement; numbering is left to the caller (the walker has no
// statement-wide cursor today). The seed assigns ordinal 0 and
// records the literal text — sufficient for plan-time wiring; the
// per-statement counter lands when the binder is plumbed.
func (r *Resolver) walkPreparedParameter(pp antlrgen.IPreparedStatementParameterContext) (values.Value, error) {
	if pp == nil {
		return nil, fmt.Errorf("expr.walkPreparedParameter: nil")
	}
	ppc, ok := pp.(*antlrgen.PreparedStatementParameterContext)
	if !ok {
		return nil, &UnsupportedExpressionShapeError{Shape: fmt.Sprintf("PreparedStatementParameter ctx %T", pp)}
	}
	if ppc.NAMED_PARAMETER() != nil {
		// Lexer rule: NAMED_PARAMETER: [?$][A-Za-z][A-Za-z0-9_/]*
		// Strip the leading sigil (`?` or `$`). Both surface forms
		// fold to the same canonical name in ParameterValue —
		// ExplainValue renders `?name` regardless of whether the
		// user wrote `?foo` or `$foo`. Intentional: one canonical
		// form for plan-cache keying.
		text := ppc.NAMED_PARAMETER().GetText()
		if len(text) < 2 || (text[0] != '?' && text[0] != '$') {
			return nil, &UnsupportedExpressionShapeError{Shape: fmt.Sprintf("NAMED_PARAMETER token %q", text)}
		}
		return values.NewNamedParameterValue(text[1:]), nil
	}
	if ppc.QUESTION() != nil {
		// 1-based ordinal, statement-scoped — matches Go's
		// database/sql NamedValue.Ordinal so binders can index by
		// position without remapping. Two `?` in the same statement
		// get distinct ordinals, so plan-cache keys derived via
		// ExplainValue normalise to `?1` / `?2` (etc.) — different
		// bind values share one cache entry, but `WHERE x=? AND y=?`
		// stays distinct from `WHERE x=?`.
		r.nextOrdinal++
		return values.NewParameterValue(r.nextOrdinal), nil
	}
	return nil, &UnsupportedExpressionShapeError{Shape: "PreparedStatementParameter with no QUESTION/NAMED_PARAMETER"}
}

// walkFunctionCall handles FunctionCall contexts. Only aggregate
// functions are wired today (COUNT/SUM/MIN/MAX/AVG); scalar function
// dispatch waits on the scalar-function catalogue port.
//
// Uses the Resolver's cached FunctionCatalog (built lazily on first
// use, or provided via NewWithFunctionCatalog) so the walker
// amortises catalog construction across calls.
func (r *Resolver) walkFunctionCall(fc antlrgen.IFunctionCallContext) (values.Value, error) {
	if fc == nil {
		return nil, fmt.Errorf("expr.walkFunctionCall: nil")
	}
	// SpecificFunctionCall covers CAST / CONVERT (DataTypeFunctionCall),
	// plus CASE / current_user / extract / etc. We only wire the
	// data-type conversions here — anything else returns an
	// UnsupportedExpressionShapeError so callers fall back cleanly.
	if spec, ok := fc.(*antlrgen.SpecificFunctionCallContext); ok {
		return r.walkSpecificFunction(spec.SpecificFunction())
	}
	if scalar, ok := fc.(*antlrgen.ScalarFunctionCallContext); ok {
		return r.walkScalarFunction(scalar)
	}
	agg, ok := fc.(*antlrgen.AggregateFunctionCallContext)
	if !ok {
		return nil, &UnsupportedExpressionShapeError{Shape: fmt.Sprintf("non-aggregate function call %T", fc)}
	}
	awf := agg.AggregateWindowedFunction()
	if awf == nil {
		return nil, &UnsupportedExpressionShapeError{Shape: "AggregateFunctionCall without AggregateWindowedFunction"}
	}
	awfc, ok := awf.(*antlrgen.AggregateWindowedFunctionContext)
	if !ok {
		return nil, &UnsupportedExpressionShapeError{Shape: fmt.Sprintf("AggregateWindowedFunction ctx %T", awf)}
	}
	name, ok := aggregateFunctionName(awfc)
	if !ok {
		return nil, &UnsupportedExpressionShapeError{Shape: "AggregateWindowedFunction with unknown operator"}
	}
	fcat := r.functionCatalog()
	// COUNT(*) — the `STAR()` accessor, which I cross-checked earlier.
	isStar := awfc.STAR() != nil
	var args []values.Value
	if !isStar && awfc.FunctionArg() != nil {
		// FunctionArg wraps an expression or a * sentinel — we only
		// walk the expression form; the star path is the isStar flag.
		argCtx, ok := awfc.FunctionArg().(*antlrgen.FunctionArgContext)
		if !ok {
			return nil, &UnsupportedExpressionShapeError{Shape: fmt.Sprintf("FunctionArg ctx %T", awfc.FunctionArg())}
		}
		if argCtx.Expression() == nil {
			return nil, &UnsupportedExpressionShapeError{Shape: "FunctionArg without Expression (star handled separately)"}
		}
		v, err := r.WalkExpression(argCtx.Expression())
		if err != nil {
			return nil, err
		}
		args = []values.Value{v}
	}
	return r.ResolveFunctionCall(fcat, semantic.NewUnquoted(name), isStar, args)
}

// walkSpecificFunction dispatches the SpecificFunction subtypes.
// Handles CAST/CONVERT (DataTypeFunctionCall), CASE (CaseFunctionCall),
// and SQL-standard datetime/user functions (SimpleFunctionCall).
func (r *Resolver) walkSpecificFunction(sf antlrgen.ISpecificFunctionContext) (values.Value, error) {
	if sf == nil {
		return nil, fmt.Errorf("expr.walkSpecificFunction: nil")
	}
	if caseCtx, ok := sf.(*antlrgen.CaseFunctionCallContext); ok {
		return r.walkCaseFunctionCall(caseCtx)
	}
	if simpleCaseCtx, ok := sf.(*antlrgen.CaseExpressionFunctionCallContext); ok {
		return r.walkSimpleCaseFunctionCall(simpleCaseCtx)
	}
	if simple, ok := sf.(*antlrgen.SimpleFunctionCallContext); ok {
		return r.walkSimpleFunctionCall(simple)
	}
	cast, ok := sf.(*antlrgen.DataTypeFunctionCallContext)
	if !ok {
		return nil, &UnsupportedExpressionShapeError{Shape: fmt.Sprintf("SpecificFunction ctx %T", sf)}
	}
	// CAST and CONVERT share the ctx; both have Expression() +
	// ConvertedDataType(). Differ only in surface syntax.
	if cast.CAST() == nil && cast.CONVERT() == nil {
		return nil, &UnsupportedExpressionShapeError{Shape: "DataTypeFunctionCall with no CAST/CONVERT token"}
	}
	exprCtx := cast.Expression()
	if exprCtx == nil {
		return nil, &UnsupportedExpressionShapeError{Shape: "CAST without Expression"}
	}
	inner, err := r.WalkExpression(exprCtx)
	if err != nil {
		return nil, err
	}
	dt := cast.ConvertedDataType()
	if dt == nil {
		return nil, &UnsupportedExpressionShapeError{Shape: "CAST without ConvertedDataType"}
	}
	dtc, ok := dt.(*antlrgen.ConvertedDataTypeContext)
	if !ok {
		return nil, &UnsupportedExpressionShapeError{Shape: fmt.Sprintf("ConvertedDataType ctx %T", dt)}
	}
	pt := dtc.PrimitiveType()
	if pt == nil {
		return nil, &UnsupportedExpressionShapeError{Shape: "ConvertedDataType without PrimitiveType"}
	}
	target, ok := primitiveTypeToValueType(pt)
	if !ok {
		return nil, &UnsupportedExpressionShapeError{
			Shape: fmt.Sprintf("CAST target not in seed Type set (INT/STRING/BOOL/FLOAT); got %q", pt.GetText()),
		}
	}
	return r.ResolveCast(inner, target)
}

// walkSimpleFunctionCall handles the SQL-standard no-argument datetime
// and user functions: CURRENT_TIMESTAMP, CURRENT_DATE, CURRENT_TIME,
// LOCALTIME, CURRENT_USER. These parse as SimpleFunctionCallContext
// (grammar: specificFunction → simpleFunctionCall). Each maps to a
// zero-arg ScalarFunctionValue whose Evaluate dispatches in
// evalScalarFunction.
func (r *Resolver) walkSimpleFunctionCall(ctx *antlrgen.SimpleFunctionCallContext) (values.Value, error) {
	switch {
	case ctx.CURRENT_TIMESTAMP() != nil:
		return values.NewScalarFunctionValue("CURRENT_TIMESTAMP", values.NullableTimestamp), nil
	case ctx.CURRENT_DATE() != nil:
		return values.NewScalarFunctionValue("CURRENT_DATE", values.NullableDate), nil
	case ctx.CURRENT_TIME() != nil:
		return values.NewScalarFunctionValue("CURRENT_TIME", values.NullableTimestamp), nil
	case ctx.LOCALTIME() != nil:
		return values.NewScalarFunctionValue("LOCALTIME", values.NullableTimestamp), nil
	case ctx.CURRENT_USER() != nil:
		return &values.ConstantValue{Value: "", Typ: values.NullableString}, nil
	default:
		return nil, &UnsupportedExpressionShapeError{Shape: "unsupported SimpleFunctionCall"}
	}
}

// walkCaseFunctionCall handles searched CASE expressions:
//
//	CASE WHEN cond1 THEN val1 WHEN cond2 THEN val2 ELSE def END
//
// Produces PickValue(ConditionSelectorValue([cond1, cond2, TRUE]), [val1, val2, def]).
// Matches Java's ExpressionVisitor.visitCaseFunctionCall.
func (r *Resolver) walkCaseFunctionCall(ctx *antlrgen.CaseFunctionCallContext) (values.Value, error) {
	if ctx == nil {
		return nil, fmt.Errorf("expr.walkCaseFunctionCall: nil")
	}
	alts := ctx.AllCaseFuncAlternative()
	implications := make([]values.Value, 0, len(alts)+1)
	alternatives := make([]values.Value, 0, len(alts)+1)

	for _, alt := range alts {
		altCtx, ok := alt.(*antlrgen.CaseFuncAlternativeContext)
		if !ok {
			return nil, &UnsupportedExpressionShapeError{Shape: fmt.Sprintf("CaseFuncAlternative ctx %T", alt)}
		}
		condArg := altCtx.GetCondition()
		if condArg == nil {
			return nil, &UnsupportedExpressionShapeError{Shape: "CASE WHEN without condition"}
		}
		condArgCtx, ok := condArg.(*antlrgen.FunctionArgContext)
		if !ok {
			return nil, &UnsupportedExpressionShapeError{Shape: fmt.Sprintf("CASE condition arg ctx %T", condArg)}
		}
		condVal, err := r.walkCaseCondition(condArgCtx.Expression())
		if err != nil {
			return nil, err
		}
		implications = append(implications, condVal)

		consArg := altCtx.GetConsequent()
		if consArg == nil {
			return nil, &UnsupportedExpressionShapeError{Shape: "CASE THEN without consequent"}
		}
		consArgCtx, ok := consArg.(*antlrgen.FunctionArgContext)
		if !ok {
			return nil, &UnsupportedExpressionShapeError{Shape: fmt.Sprintf("CASE consequent arg ctx %T", consArg)}
		}
		consVal, err := r.WalkExpression(consArgCtx.Expression())
		if err != nil {
			return nil, err
		}
		alternatives = append(alternatives, consVal)
	}

	if ctx.ELSE() != nil {
		implications = append(implications, values.NewBooleanValue(true))
		elseArg := ctx.GetElseArg()
		if elseArg == nil {
			return nil, &UnsupportedExpressionShapeError{Shape: "CASE ELSE without arg"}
		}
		elseArgCtx, ok := elseArg.(*antlrgen.FunctionArgContext)
		if !ok {
			return nil, &UnsupportedExpressionShapeError{Shape: fmt.Sprintf("CASE ELSE arg ctx %T", elseArg)}
		}
		elseVal, err := r.WalkExpression(elseArgCtx.Expression())
		if err != nil {
			return nil, err
		}
		alternatives = append(alternatives, elseVal)
	}

	selector := values.NewConditionSelectorValue(implications)
	return values.NewPickValue(selector, alternatives, values.UnknownType), nil
}

// walkSimpleCaseFunctionCall handles simple CASE expressions:
//
//	CASE expr WHEN val1 THEN res1 WHEN val2 THEN res2 ELSE def END
//
// Desugars to: PickValue(ConditionSelectorValue([expr=val1, expr=val2, TRUE]), [res1, res2, def])
// where each implication is a comparison predicate (expr = valN).
func (r *Resolver) walkSimpleCaseFunctionCall(ctx *antlrgen.CaseExpressionFunctionCallContext) (values.Value, error) {
	if ctx == nil {
		return nil, fmt.Errorf("expr.walkSimpleCaseFunctionCall: nil")
	}
	discriminator, err := r.WalkExpression(ctx.Expression())
	if err != nil {
		return nil, err
	}

	alts := ctx.AllCaseFuncAlternative()
	implications := make([]values.Value, 0, len(alts)+1)
	alternatives := make([]values.Value, 0, len(alts)+1)

	for _, alt := range alts {
		altCtx, ok := alt.(*antlrgen.CaseFuncAlternativeContext)
		if !ok {
			return nil, &UnsupportedExpressionShapeError{Shape: fmt.Sprintf("CaseFuncAlternative ctx %T", alt)}
		}
		condArg := altCtx.GetCondition()
		if condArg == nil {
			return nil, &UnsupportedExpressionShapeError{Shape: "simple CASE WHEN without condition"}
		}
		condArgCtx, ok := condArg.(*antlrgen.FunctionArgContext)
		if !ok {
			return nil, &UnsupportedExpressionShapeError{Shape: fmt.Sprintf("simple CASE condition arg ctx %T", condArg)}
		}
		whenVal, err := r.WalkExpression(condArgCtx.Expression())
		if err != nil {
			return nil, err
		}
		eqPred, err := r.ResolveComparison(predicates.ComparisonEquals, discriminator, whenVal)
		if err != nil {
			return nil, err
		}
		implications = append(implications, &predicateValue{pred: eqPred})

		consArg := altCtx.GetConsequent()
		if consArg == nil {
			return nil, &UnsupportedExpressionShapeError{Shape: "simple CASE THEN without consequent"}
		}
		consArgCtx, ok := consArg.(*antlrgen.FunctionArgContext)
		if !ok {
			return nil, &UnsupportedExpressionShapeError{Shape: fmt.Sprintf("simple CASE consequent arg ctx %T", consArg)}
		}
		consVal, err := r.WalkExpression(consArgCtx.Expression())
		if err != nil {
			return nil, err
		}
		alternatives = append(alternatives, consVal)
	}

	if ctx.ELSE() != nil {
		implications = append(implications, values.NewBooleanValue(true))
		elseArg := ctx.GetElseArg()
		if elseArg == nil {
			return nil, &UnsupportedExpressionShapeError{Shape: "simple CASE ELSE without arg"}
		}
		elseArgCtx, ok := elseArg.(*antlrgen.FunctionArgContext)
		if !ok {
			return nil, &UnsupportedExpressionShapeError{Shape: fmt.Sprintf("simple CASE ELSE arg ctx %T", elseArg)}
		}
		elseVal, err := r.WalkExpression(elseArgCtx.Expression())
		if err != nil {
			return nil, err
		}
		alternatives = append(alternatives, elseVal)
	}

	selector := values.NewConditionSelectorValue(implications)
	return values.NewPickValue(selector, alternatives, values.UnknownType), nil
}

// walkCaseCondition resolves a CASE WHEN condition expression. The
// condition can be either a plain value (boolean column) or a
// predicate (comparison like `score = 0`). Returns a Value that
// evaluates to boolean for use in ConditionSelectorValue.
func (r *Resolver) walkCaseCondition(ctx antlrgen.IExpressionContext) (values.Value, error) {
	v, err := r.WalkExpression(ctx)
	if err == nil {
		return v, nil
	}
	pred, predErr := r.WalkPredicate(ctx)
	if predErr != nil {
		return nil, err
	}
	return &predicateValue{pred: pred}, nil
}

// PredicateValueHolder is implemented by values that wrap a
// QueryPredicate (used by CASE conditions). Exported so the
// aggregate-rewriter in logical_predicate.go can access the
// predicate for AggregateValue→FieldValue rewriting.
type PredicateValueHolder interface {
	values.Value
	GetPredicate() predicates.QueryPredicate
	SetPredicate(predicates.QueryPredicate)
}

// predicateValue wraps a QueryPredicate as a Value for use in CASE
// conditions. Evaluates to true/false/nil (SQL 3VL).
type predicateValue struct {
	pred predicates.QueryPredicate
}

func (pv *predicateValue) Children() []values.Value                 { return []values.Value{} }
func (pv *predicateValue) Name() string                             { return "predicate" }
func (pv *predicateValue) Type() values.Type                        { return values.TypeBool }
func (pv *predicateValue) GetPredicate() predicates.QueryPredicate  { return pv.pred }
func (pv *predicateValue) SetPredicate(p predicates.QueryPredicate) { pv.pred = p }

func (pv *predicateValue) Evaluate(evalCtx any) any {
	if pv.pred == nil {
		return nil
	}
	switch pv.pred.Eval(evalCtx) {
	case predicates.TriTrue:
		return true
	case predicates.TriFalse:
		return false
	default:
		return nil
	}
}

// walkScalarFunction handles every scalar function name registered
// in scalarFunctionResultType() (the source of truth — see that
// function for the live list). Unknown function names decline with
// UnsupportedExpressionShapeError so the logical-builder text
// fallback catches them; this keeps the walker conservative until
// the full scalar function catalogue ports.
//
// Args walk through the standard WalkExpression dispatch so nested
// expressions (`UPPER(name)`, `LENGTH(CAST(x AS STRING))`) compose
// without further plumbing.
func (r *Resolver) walkScalarFunction(s *antlrgen.ScalarFunctionCallContext) (values.Value, error) {
	if s == nil {
		return nil, fmt.Errorf("expr.walkScalarFunction: nil")
	}
	if s.ScalarFunctionName() == nil {
		return nil, &UnsupportedExpressionShapeError{Shape: "ScalarFunctionCall without ScalarFunctionName"}
	}
	name := strings.ToUpper(s.ScalarFunctionName().GetText())
	typ, ok := scalarFunctionResultType(name)
	if !ok {
		return nil, &UnsupportedExpressionShapeError{Shape: fmt.Sprintf("scalar function %q (not in seed catalogue)", name)}
	}
	args := []values.Value{}
	if fa := s.FunctionArgs(); fa != nil {
		fac, ok := fa.(*antlrgen.FunctionArgsContext)
		if !ok {
			return nil, &UnsupportedExpressionShapeError{Shape: fmt.Sprintf("FunctionArgs ctx %T", fa)}
		}
		for _, arg := range fac.AllFunctionArg() {
			argCtx, ok := arg.(*antlrgen.FunctionArgContext)
			if !ok {
				return nil, &UnsupportedExpressionShapeError{Shape: fmt.Sprintf("FunctionArg ctx %T", arg)}
			}
			if argCtx.Expression() == nil {
				return nil, &UnsupportedExpressionShapeError{Shape: "FunctionArg without Expression"}
			}
			v, err := r.WalkExpression(argCtx.Expression())
			if err != nil {
				return nil, err
			}
			args = append(args, v)
		}
	}
	return values.NewScalarFunctionValue(name, typ, args...), nil
}

// scalarFunctionResultType returns the result type of a seed scalar
// function. Unknown name → (_, false) so the walker declines.
//
// Polymorphic returns (ABS / CEILING / FLOOR / ROUND / COALESCE /
// NULLIF) carry UnknownType because the result type depends on the
// input — int input stays int, float input stays float. Real
// per-arg inference is future work.
func scalarFunctionResultType(name string) (values.Type, bool) {
	switch name {
	case "UPPER", "LOWER", "TRIM", "LTRIM", "RTRIM",
		"CONCAT", "CONCAT_WS", "SUBSTRING", "SUBSTR", "REPLACE",
		"REVERSE", "LEFT", "RIGHT":
		return values.TypeString, true
	case "LENGTH", "LEN", "CHAR_LENGTH", "CHARACTER_LENGTH", "OCTET_LENGTH",
		"POSITION",
		"YEAR", "MONTH", "DAY", "DAYOFMONTH",
		"HOUR", "MINUTE", "SECOND",
		"DAYOFWEEK", "DAYOFYEAR":
		return values.TypeInt, true
	case "SQRT", "POWER", "POW", "EXP", "LN", "LOG", "PI":
		return values.TypeFloat, true
	case "ABS", "FLOOR", "CEIL", "CEILING", "ROUND",
		"SIGN", "MOD",
		"COALESCE", "NULLIF", "IFNULL",
		"IF", "IIF", "GREATEST", "LEAST":
		return values.TypeUnknown, true
	}
	return values.TypeUnknown, false
}

// primitiveTypeToValueType maps the PrimitiveType terminal to a
// values.Type. BYTES / UUID / VECTOR aren't in the seed CAST set —
// they return (_, false) so the walker declines.
func primitiveTypeToValueType(pt antlrgen.IPrimitiveTypeContext) (values.Type, bool) {
	ptc, ok := pt.(*antlrgen.PrimitiveTypeContext)
	if !ok {
		return values.TypeUnknown, false
	}
	switch {
	case ptc.INTEGER() != nil:
		return values.NullableInt, true
	case ptc.BIGINT() != nil:
		return values.NullableLong, true
	case ptc.STRING() != nil:
		return values.TypeString, true
	case ptc.BOOLEAN() != nil:
		return values.TypeBool, true
	case ptc.FLOAT() != nil, ptc.DOUBLE() != nil:
		return values.TypeFloat, true
	case ptc.DATE() != nil:
		return values.NullableDate, true
	case ptc.TIMESTAMP() != nil:
		return values.NullableTimestamp, true
	}
	return values.TypeUnknown, false
}

// aggregateFunctionName reads which terminal is present on the
// AggregateWindowedFunction context and returns the canonical
// UPPER-case name.
func aggregateFunctionName(awf *antlrgen.AggregateWindowedFunctionContext) (string, bool) {
	switch {
	case awf.COUNT() != nil:
		return "COUNT", true
	case awf.SUM() != nil:
		return "SUM", true
	case awf.MIN() != nil:
		return "MIN", true
	case awf.MAX() != nil:
		return "MAX", true
	case awf.AVG() != nil:
		return "AVG", true
	}
	return "", false
}

// walkMathExpression walks an arithmetic atom (`a + b`, `a * b`)
// into a values.ArithmeticValue. Operator resolution reads the
// MathOperator context's terminal tokens. MOD / MODULE / DIV
// (integer division) aren't mapped to values.ArithmeticOp yet —
// the cascades enum covers +, -, *, / and grows with the Type
// hierarchy port.
func (r *Resolver) walkMathExpression(m *antlrgen.MathExpressionAtomContext) (values.Value, error) {
	op, err := arithmeticOpFromCtx(m.MathOperator())
	if err != nil {
		return nil, err
	}
	left, err := r.walkAtom(m.GetLeft())
	if err != nil {
		return nil, err
	}
	right, err := r.walkAtom(m.GetRight())
	if err != nil {
		return nil, err
	}
	return r.ResolveArithmetic(op, left, right)
}

// arithmeticOpFromCtx reads the MathOperator terminal tokens.
// Returns UnsupportedExpressionShapeError for operators not yet
// present in values.ArithmeticOp.
func arithmeticOpFromCtx(op antlrgen.IMathOperatorContext) (values.ArithmeticOp, error) {
	if op == nil {
		return values.OpAdd, fmt.Errorf("arithmeticOpFromCtx: nil")
	}
	mo, ok := op.(*antlrgen.MathOperatorContext)
	if !ok {
		return values.OpAdd, fmt.Errorf("arithmeticOpFromCtx: unexpected ctx %T", op)
	}
	switch {
	case mo.PLUS() != nil:
		return values.OpAdd, nil
	case mo.MINUS() != nil:
		return values.OpSub, nil
	case mo.STAR() != nil:
		return values.OpMul, nil
	case mo.DIVIDE() != nil, mo.DIV() != nil:
		// `/` and `DIV` both map to OpDiv. In MySQL `DIV` is the
		// integer-truncated division operator while `/` is true
		// division — at the seed they coincide because
		// ArithmeticValue.Evaluate is int64-only and Go's `/` on int64
		// already truncates toward zero. Once the Type hierarchy
		// lands and float arithmetic is wired, DIV will need its own
		// op (OpIntDiv) that coerces float operands to int before
		// dividing.
		return values.OpDiv, nil
	case mo.MOD() != nil, mo.MODULE() != nil:
		// MOD / MODULE / `%` all map to OpMod. The grammar treats
		// `MOD` as a keyword and `MODULE` as the synonym, plus `%`
		// as the operator (covered by mo.MODULE() in this grammar).
		return values.OpMod, nil
	}
	return values.OpAdd, &UnsupportedExpressionShapeError{Shape: "MathOperator: " + mo.GetText()}
}

// walkBitExpression handles bitwise operators (`&`, `|`, `^`, `<<`, `>>`).
// Produces a ScalarFunctionValue with the canonical operator name so
// evalScalarFunction can dispatch it.
func (r *Resolver) walkBitExpression(b *antlrgen.BitExpressionAtomContext) (values.Value, error) {
	left, err := r.walkAtom(b.GetLeft())
	if err != nil {
		return nil, err
	}
	right, err := r.walkAtom(b.GetRight())
	if err != nil {
		return nil, err
	}
	bo := b.BitOperator()
	if bo == nil {
		return nil, &UnsupportedExpressionShapeError{Shape: "BitExpressionAtom with nil operator"}
	}
	opText := bo.GetText()
	name := "BITAND"
	switch opText {
	case "&":
		name = "BITAND"
	case "|":
		name = "BITOR"
	case "^":
		name = "BITXOR"
	case "<<":
		name = "BITSHL"
	case ">>":
		name = "BITSHR"
	default:
		return nil, &UnsupportedExpressionShapeError{Shape: "BitOperator: " + opText}
	}
	return values.NewScalarFunctionValue(name, values.TypeInt, left, right), nil
}

// walkRecordConstructor unwraps a single-element, unnamed-field,
// un-typed record constructor — the parser's shape for
// parenthesised expressions `(expr)`. Multi-element or annotated
// record constructors require dedicated values.RecordConstructorValue
// support, not wired yet.
func (r *Resolver) walkRecordConstructor(rc antlrgen.IRecordConstructorContext) (values.Value, error) {
	if rc == nil {
		return nil, fmt.Errorf("expr.walkRecordConstructor: nil")
	}
	rcc, ok := rc.(*antlrgen.RecordConstructorContext)
	if !ok {
		return nil, &UnsupportedExpressionShapeError{Shape: fmt.Sprintf("RecordConstructor ctx %T", rc)}
	}
	exprs := rcc.AllExpressionWithOptionalName()
	if len(exprs) != 1 {
		return nil, &UnsupportedExpressionShapeError{
			Shape: fmt.Sprintf("RecordConstructor with %d elements; walker handles 1-elem (paren expr) only", len(exprs)),
		}
	}
	ewon, ok := exprs[0].(*antlrgen.ExpressionWithOptionalNameContext)
	if !ok {
		return nil, &UnsupportedExpressionShapeError{Shape: fmt.Sprintf("ExpressionWithOptionalName ctx %T", exprs[0])}
	}
	if ewon.Uid() != nil {
		// Named field — real record constructor, not a paren-wrap.
		return nil, &UnsupportedExpressionShapeError{Shape: "RecordConstructor with named field"}
	}
	if rcc.OfTypeClause() != nil {
		return nil, &UnsupportedExpressionShapeError{Shape: "RecordConstructor with OfType"}
	}
	return r.WalkExpression(ewon.Expression())
}

// WalkPredicate is the dual of WalkExpression — returns a cascades
// QueryPredicate for an expression that's semantically boolean.
// Handles:
//
//   - LogicalExpression: `a AND b`, `a OR b` → And/Or predicate.
//     NOT is a grammar-level unary-expression, handled separately.
//   - PredicatedExpression with BinaryComparisonPredicate atom →
//     ComparisonPredicate.
//   - PredicatedExpression with a plain value atom → ValuePredicate
//     (bare boolean column like `WHERE flag`).
//
// Other shapes (BETWEEN, IN, LIKE, IS NULL via grammar's Predicate
// node; NOT; XOR) return UnsupportedExpressionShapeError.
func (r *Resolver) WalkPredicate(ctx antlrgen.IExpressionContext) (predicates.QueryPredicate, error) {
	if ctx == nil {
		return nil, fmt.Errorf("expr.WalkPredicate: nil context")
	}
	switch c := ctx.(type) {
	case *antlrgen.PredicatedExpressionContext:
		return r.walkPredicatedExpression(c)
	case *antlrgen.LogicalExpressionContext:
		return r.walkLogicalExpression(c)
	case *antlrgen.NotExpressionContext:
		child, err := r.WalkPredicate(c.Expression())
		if err != nil {
			return nil, err
		}
		return r.ResolveNot(child), nil
	case *antlrgen.ExistsExpressionAtomContext:
		return r.walkExistsPredicate(c)
	}
	return nil, &UnsupportedExpressionShapeError{Shape: fmt.Sprintf("%T", ctx)}
}

// walkPredicatedExpression handles the leaf case — a bare value or
// a BinaryComparisonPredicate atom, plus grammar Predicate shapes
// (IS NULL / IS NOT NULL) that modify the preceding atom.
func (r *Resolver) walkPredicatedExpression(pred *antlrgen.PredicatedExpressionContext) (predicates.QueryPredicate, error) {
	if p := pred.Predicate(); p != nil {
		return r.walkGrammarPredicate(pred.ExpressionAtom(), p)
	}
	atom := pred.ExpressionAtom()
	if bc, ok := atom.(*antlrgen.BinaryComparisonPredicateContext); ok {
		return r.walkBinaryComparison(bc)
	}
	// Parenthesised predicate: `(cond)` surfaces as a RecordConstructor
	// atom with one unnamed child. Try the predicate-form recursion
	// first; if it turns out not to be predicate-shaped (just a bare
	// paren-wrapped value), fall through to the value path.
	if rc, ok := atom.(*antlrgen.RecordConstructorExpressionAtomContext); ok {
		if inner, err := r.unwrapParenPredicate(rc.RecordConstructor()); err == nil {
			return inner, nil
		}
		// Otherwise fall through — rc might be a bare paren-wrapped
		// value (unusual in WHERE but legal syntactically).
	}
	v, err := r.walkAtom(atom)
	if err != nil {
		return nil, err
	}
	// BooleanValue with a concrete TRUE/FALSE → ConstantPredicate,
	// not ValuePredicate. This is what the simplifier's constant-
	// fold rules expect to see; a ValuePredicate-wrapped boolean
	// would be treated as opaque and not simplified.
	if bv, ok := v.(*values.BooleanValue); ok && bv.Value != nil {
		if *bv.Value {
			return predicates.NewConstantPredicate(predicates.TriTrue), nil
		}
		return predicates.NewConstantPredicate(predicates.TriFalse), nil
	}
	return predicates.NewValuePredicate(v), nil
}

// unwrapParenPredicate recurses WalkPredicate on a single-element
// parenthesised expression. Returns error if the RecordConstructor
// shape isn't a simple paren-wrap (multi-element, named field,
// OfType clause).
func (r *Resolver) unwrapParenPredicate(rc antlrgen.IRecordConstructorContext) (predicates.QueryPredicate, error) {
	if rc == nil {
		return nil, fmt.Errorf("unwrapParenPredicate: nil")
	}
	rcc, ok := rc.(*antlrgen.RecordConstructorContext)
	if !ok {
		return nil, &UnsupportedExpressionShapeError{Shape: fmt.Sprintf("RecordConstructor ctx %T", rc)}
	}
	exprs := rcc.AllExpressionWithOptionalName()
	if len(exprs) != 1 || rcc.OfTypeClause() != nil {
		return nil, &UnsupportedExpressionShapeError{Shape: "RecordConstructor not a 1-elem paren-wrap"}
	}
	ewon, ok := exprs[0].(*antlrgen.ExpressionWithOptionalNameContext)
	if !ok {
		return nil, &UnsupportedExpressionShapeError{Shape: fmt.Sprintf("ExpressionWithOptionalName ctx %T", exprs[0])}
	}
	if ewon.Uid() != nil {
		return nil, &UnsupportedExpressionShapeError{Shape: "RecordConstructor with named field"}
	}
	return r.WalkPredicate(ewon.Expression())
}

// walkLogicalExpression builds a cascades And/Or predicate from a
// LogicalExpression (`a AND b` / `a OR b`). Left-associative chains
// in the grammar mean `a AND b AND c` nests as `(a AND b) AND c`;
// we flatten on the fly when the LHS is already the same kind.
func (r *Resolver) walkLogicalExpression(le *antlrgen.LogicalExpressionContext) (predicates.QueryPredicate, error) {
	op := le.LogicalOperator()
	if op == nil {
		return nil, &UnsupportedExpressionShapeError{Shape: "LogicalExpression with nil operator"}
	}
	lo, ok := op.(*antlrgen.LogicalOperatorContext)
	if !ok {
		return nil, &UnsupportedExpressionShapeError{Shape: fmt.Sprintf("LogicalOperator ctx %T", op)}
	}
	exprs := le.AllExpression()
	if len(exprs) != 2 {
		return nil, &UnsupportedExpressionShapeError{
			Shape: fmt.Sprintf("LogicalExpression with %d children; expected 2", len(exprs)),
		}
	}
	left, err := r.WalkPredicate(exprs[0])
	if err != nil {
		return nil, err
	}
	right, err := r.WalkPredicate(exprs[1])
	if err != nil {
		return nil, err
	}
	switch {
	case lo.AND() != nil || len(lo.AllBIT_AND_OP()) == 2:
		return r.ResolveAnd(flattenAnd(left, right)...), nil
	case lo.OR() != nil || len(lo.AllBIT_OR_OP()) == 2:
		return r.ResolveOr(flattenOr(left, right)...), nil
	case lo.XOR() != nil:
		// Desugar `a XOR b` → `(a OR b) AND NOT (a AND b)`.
		// This is exact under Kleene 3VL: both sides evaluate to
		// UNKNOWN iff either input is UNKNOWN, which is the expected
		// behaviour for NULL XOR x.
		or := r.ResolveOr(flattenOr(left, right)...)
		and := r.ResolveAnd(flattenAnd(left, right)...)
		return r.ResolveAnd(or, r.ResolveNot(and)), nil
	}
	return nil, &UnsupportedExpressionShapeError{Shape: "LogicalOperator: " + lo.GetText()}
}

// flattenAnd/flattenOr collapse left-deep chains built by the
// parser. `a AND b AND c` parses as (and (and a b) c) — here we
// return [a b c] so ResolveAnd produces a single 3-child And
// rather than nested pairs. AndFlattenRule in cascades would fix
// it later anyway, but seeding the flat shape avoids fixpoint work.
func flattenAnd(preds ...predicates.QueryPredicate) []predicates.QueryPredicate {
	var out []predicates.QueryPredicate
	for _, p := range preds {
		if and, ok := p.(*predicates.AndPredicate); ok {
			out = append(out, and.SubPredicates...)
		} else {
			out = append(out, p)
		}
	}
	return out
}

func flattenOr(preds ...predicates.QueryPredicate) []predicates.QueryPredicate {
	var out []predicates.QueryPredicate
	for _, p := range preds {
		if or, ok := p.(*predicates.OrPredicate); ok {
			out = append(out, or.SubPredicates...)
		} else {
			out = append(out, p)
		}
	}
	return out
}

// walkGrammarPredicate handles PredicateContext shapes that modify
// a preceding atom — IS [NOT] NULL today, BETWEEN / IN / LIKE in
// follow-up commits.
func (r *Resolver) walkGrammarPredicate(atom antlrgen.IExpressionAtomContext, pred antlrgen.IPredicateContext) (predicates.QueryPredicate, error) {
	if atom == nil {
		return nil, fmt.Errorf("expr.walkGrammarPredicate: nil atom")
	}
	switch p := pred.(type) {
	case *antlrgen.IsExpressionContext:
		lhs, err := r.walkAtom(atom)
		if err != nil {
			return nil, err
		}
		switch {
		case p.NULL_LITERAL() != nil:
			if p.NOT() != nil {
				return r.ResolveIsNotNull(lhs)
			}
			return r.ResolveIsNull(lhs)
		case p.TRUE() != nil:
			return r.resolveIsBoolean(lhs, true, p.NOT() != nil)
		case p.FALSE() != nil:
			return r.resolveIsBoolean(lhs, false, p.NOT() != nil)
		}
		return nil, &UnsupportedExpressionShapeError{Shape: "IS expression with no recognised literal"}
	case *antlrgen.InPredicateContext:
		// `x IN (a, b, c)` via the Expressions branch of InList.
		// Subquery / parameter / single-column forms not wired yet.
		il := p.InList()
		if il == nil {
			return nil, &UnsupportedExpressionShapeError{Shape: "InPredicate with nil InList"}
		}
		ilc, ok := il.(*antlrgen.InListContext)
		if !ok {
			return nil, &UnsupportedExpressionShapeError{Shape: fmt.Sprintf("InList ctx %T", il)}
		}
		exprs := ilc.Expressions()
		if exprs == nil {
			if ilc.QueryExpressionBody() != nil {
				return nil, &UnsupportedExpressionShapeError{Shape: "IN with subquery"}
			}
			return nil, &InColumnRefError{}
		}
		ec, ok := exprs.(*antlrgen.ExpressionsContext)
		if !ok {
			return nil, &UnsupportedExpressionShapeError{Shape: fmt.Sprintf("Expressions ctx %T", exprs)}
		}
		lhsVal, err := r.walkAtom(atom)
		if err != nil {
			return nil, err
		}
		list := make([]values.Value, 0, len(ec.AllExpression()))
		for _, e := range ec.AllExpression() {
			v, err := r.WalkExpression(e)
			if err != nil {
				return nil, err
			}
			if _, isNull := v.(*values.NullValue); isNull {
				return nil, &InListNullError{}
			}
			if cv, isCon := v.(*values.ConstantValue); isCon && cv.Value == nil {
				return nil, &InListNullError{}
			}
			list = append(list, v)
		}
		inPred, err := r.ResolveIn(lhsVal, list)
		if err != nil {
			return nil, err
		}
		if p.NOT() != nil {
			return r.ResolveNot(inPred), nil
		}
		return inPred, nil
	case *antlrgen.LikePredicateContext:
		// `x LIKE 'pattern' [ESCAPE 'c']` — both forms wire through
		// the cascades likeMatch's escape-aware path. ESCAPE='' or
		// missing ESCAPE produces escape == 0, which disables
		// escape handling.
		lhsVal, err := r.walkAtom(atom)
		if err != nil {
			return nil, err
		}
		patTok := p.GetPattern()
		if patTok == nil {
			return nil, &UnsupportedExpressionShapeError{Shape: "LIKE without pattern token"}
		}
		patConst, err := r.ResolveConstant(stripStringLiteral(patTok.GetText()))
		if err != nil {
			return nil, err
		}
		var escape rune
		if p.ESCAPE() != nil {
			escTok := p.GetEscape()
			if escTok == nil {
				return nil, &UnsupportedExpressionShapeError{Shape: "LIKE ESCAPE without escape token"}
			}
			escStr := stripStringLiteral(escTok.GetText())
			runes := []rune(escStr)
			if len(runes) != 1 {
				return nil, &UnsupportedExpressionShapeError{
					Shape: fmt.Sprintf("LIKE ESCAPE expects exactly one character; got %q", escStr),
				}
			}
			escape = runes[0]
		}
		like, err := r.ResolveLikeWithEscape(lhsVal, patConst, escape)
		if err != nil {
			return nil, err
		}
		if p.NOT() != nil {
			return r.ResolveNot(like), nil
		}
		return like, nil
	case *antlrgen.BetweenComparisonPredicateContext:
		// `x BETWEEN lo AND hi` → x >= lo AND x <= hi.
		// `x NOT BETWEEN lo AND hi` → NOT (x >= lo AND x <= hi),
		// which the NotComparisonRewrite rule will canonicalise.
		lhsVal, err := r.walkAtom(atom)
		if err != nil {
			return nil, err
		}
		loVal, err := r.walkAtom(p.GetLeft())
		if err != nil {
			return nil, err
		}
		hiVal, err := r.walkAtom(p.GetRight())
		if err != nil {
			return nil, err
		}
		lowerBound, err := r.ResolveComparison(predicates.ComparisonGreaterThanEq, lhsVal, loVal)
		if err != nil {
			return nil, err
		}
		upperBound, err := r.ResolveComparison(predicates.ComparisonLessThanOrEq, lhsVal, hiVal)
		if err != nil {
			return nil, err
		}
		between := r.ResolveAnd(lowerBound, upperBound)
		if p.NOT() != nil {
			return r.ResolveNot(between), nil
		}
		return between, nil
	}
	return nil, &UnsupportedExpressionShapeError{Shape: fmt.Sprintf("grammar Predicate %T", pred)}
}

// resolveIsBoolean desugars `x IS [NOT] TRUE` and `x IS [NOT] FALSE`
// into the 2VL-correct form `(x IS NOT NULL) AND (x = literal)` (and
// NOT-wrapped for the negated form). The naive `x = TRUE` differs
// from SQL's `x IS TRUE` on NULL inputs:
//
//	NULL = TRUE  → UNKNOWN   (Kleene 3VL)
//	NULL IS TRUE → FALSE     (SQL 2VL — definite)
//
// The difference is invisible at the WHERE top level (UNKNOWN is
// treated as not-selected, same as FALSE) but diverges once the
// predicate is nested under NOT, OR, or any downstream expression
// that distinguishes UNKNOWN from FALSE. The explicit null-check
// forces the correct 2VL outcome.
func (r *Resolver) resolveIsBoolean(lhs values.Value, literal, negated bool) (predicates.QueryPredicate, error) {
	notNull, err := r.ResolveIsNotNull(lhs)
	if err != nil {
		return nil, err
	}
	eq := predicates.NewComparisonPredicate(lhs, predicates.Comparison{
		Type: predicates.ComparisonEquals, Operand: values.NewBooleanValue(literal),
	})
	isBool := r.ResolveAnd(notNull, eq)
	if negated {
		return r.ResolveNot(isBool), nil
	}
	return isBool, nil
}

// walkBinaryComparison converts `left OP right` into a
// ComparisonPredicate via ResolveComparison. Operator dispatch
// reads ComparisonOperator's terminal-token accessors — there's no
// single GetText we can rely on since `!=`, `<>`, `>=` all span
// two tokens.
func (r *Resolver) walkBinaryComparison(bc *antlrgen.BinaryComparisonPredicateContext) (predicates.QueryPredicate, error) {
	op, err := comparisonOpFromCtx(bc.ComparisonOperator())
	if err != nil {
		return nil, err
	}
	left, err := r.walkAtom(bc.GetLeft())
	if err != nil {
		return nil, err
	}
	right, err := r.walkAtom(bc.GetRight())
	if err != nil {
		return nil, err
	}
	return r.ResolveComparison(op, left, right)
}

// comparisonOpFromCtx reads the terminal tokens on a
// ComparisonOperator context to identify the operator. Mirrors
// the grammar:
//
//	= | > | < | >= | <= | <> | != | IS [NOT] DISTINCT FROM
func comparisonOpFromCtx(op antlrgen.IComparisonOperatorContext) (predicates.ComparisonType, error) {
	if op == nil {
		return predicates.ComparisonEquals, fmt.Errorf("comparisonOpFromCtx: nil operator")
	}
	c, ok := op.(*antlrgen.ComparisonOperatorContext)
	if !ok {
		return predicates.ComparisonEquals, fmt.Errorf("comparisonOpFromCtx: unexpected ctx %T", op)
	}
	hasEq := c.EQUAL_SYMBOL() != nil
	hasGt := c.GREATER_SYMBOL() != nil
	hasLt := c.LESS_SYMBOL() != nil
	hasNot := c.EXCLAMATION_SYMBOL() != nil
	// Spread multi-token operators. Token order matters — the
	// grammar emits <= as '<' '=', not '=' '<'.
	if c.IS() != nil && c.DISTINCT() != nil && c.FROM() != nil {
		if c.NOT() != nil {
			return predicates.ComparisonNotDistinctFrom, nil
		}
		return predicates.ComparisonIsDistinctFrom, nil
	}
	switch {
	case hasEq && !hasGt && !hasLt && !hasNot:
		return predicates.ComparisonEquals, nil
	case hasNot && hasEq:
		return predicates.ComparisonNotEquals, nil
	case hasLt && hasGt: // <>
		return predicates.ComparisonNotEquals, nil
	case hasLt && hasEq:
		return predicates.ComparisonLessThanOrEq, nil
	case hasGt && hasEq:
		return predicates.ComparisonGreaterThanEq, nil
	case hasLt:
		return predicates.ComparisonLessThan, nil
	case hasGt:
		return predicates.ComparisonGreaterThan, nil
	}
	return predicates.ComparisonEquals, &UnsupportedExpressionShapeError{
		Shape: "ComparisonOperator: " + c.GetText(),
	}
}

// walkColumnRef: an identifier from the parse tree → ResolveIdentifier.
// Handles both bare (`col`) and qualified (`t.col`) via the number
// of Uid segments.
func (r *Resolver) walkColumnRef(fullId antlrgen.IFullIdContext) (values.Value, error) {
	if fullId == nil {
		return nil, fmt.Errorf("expr.walkColumnRef: nil FullId")
	}
	uids := fullId.AllUid()
	switch len(uids) {
	case 1:
		return r.ResolveIdentifier(
			semantic.Identifier{},
			semantic.FromUidContext(uids[0], r.analyzer.CaseSensitive()),
		)
	case 2:
		return r.ResolveIdentifier(
			semantic.FromUidContext(uids[0], r.analyzer.CaseSensitive()),
			semantic.FromUidContext(uids[1], r.analyzer.CaseSensitive()),
		)
	}
	// Deeper-qualified references (`schema.table.col`) need extra
	// resolution; defer until the logical-builder needs them.
	return nil, &UnsupportedExpressionShapeError{
		Shape: fmt.Sprintf("FullId with %d segments", len(uids)),
	}
}

// walkConstant: a literal from the parse tree → ResolveConstant.
// Handles integer / float / string / boolean / NULL constants.
// DecimalConstant covers both DECIMAL_LITERAL (int) and REAL_LITERAL
// (float) — DecimalLiteralContext exposes both terminal accessors
// and we dispatch by which is set.
func (r *Resolver) walkConstant(c antlrgen.IConstantContext) (values.Value, error) {
	if c == nil {
		return nil, fmt.Errorf("expr.walkConstant: nil Constant")
	}
	switch k := c.(type) {
	case *antlrgen.NullConstantContext:
		return r.ResolveConstant(nil)
	case *antlrgen.BooleanConstantContext:
		bl, ok := k.BooleanLiteral().(*antlrgen.BooleanLiteralContext)
		if !ok {
			return nil, &UnsupportedExpressionShapeError{Shape: fmt.Sprintf("BooleanLiteral ctx %T", k.BooleanLiteral())}
		}
		switch {
		case bl.TRUE() != nil:
			return r.ResolveConstant(true)
		case bl.FALSE() != nil:
			return r.ResolveConstant(false)
		}
		return nil, &UnsupportedExpressionShapeError{Shape: "BooleanLiteral with no TRUE/FALSE"}
	case *antlrgen.DecimalConstantContext:
		// DecimalLiteralContext wraps either DECIMAL_LITERAL (int)
		// or REAL_LITERAL (float). Distinguish by which terminal is
		// non-nil. Fall back to int parse when the literal node is
		// missing (defensive — shouldn't happen).
		if dl, ok := k.DecimalLiteral().(*antlrgen.DecimalLiteralContext); ok {
			if dl.REAL_LITERAL() != nil {
				text := k.GetText()
				f, err := strconv.ParseFloat(text, 64)
				if err != nil {
					return nil, &NumericOverflowLiteralError{Text: text}
				}
				return r.ResolveConstant(f)
			}
		}
		text := k.GetText()
		n, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("expr.walkConstant: integer parse %q: %w", text, err)
		}
		return r.ResolveConstant(n)
	case *antlrgen.NegativeDecimalConstantContext:
		// `-N` constant — same DecimalLiteral wrapper but with a
		// leading MINUS. Dispatch on REAL vs DECIMAL again.
		text := k.GetText()
		if dl, ok := k.DecimalLiteral().(*antlrgen.DecimalLiteralContext); ok {
			if dl.REAL_LITERAL() != nil {
				f, err := strconv.ParseFloat(text, 64)
				if err != nil {
					return nil, &NumericOverflowLiteralError{Text: text}
				}
				return r.ResolveConstant(f)
			}
		}
		n, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("expr.walkConstant: negative integer parse %q: %w", text, err)
		}
		return r.ResolveConstant(n)
	case *antlrgen.StringConstantContext:
		// Grammar emits the literal including surrounding quotes;
		// strip them. Only single-quoted SQL strings for now.
		text := k.GetText()
		if len(text) >= 2 && text[0] == '\'' && text[len(text)-1] == '\'' {
			text = strings.ReplaceAll(text[1:len(text)-1], "''", "'")
		}
		return r.ResolveConstant(text)
	case *antlrgen.BytesConstantContext:
		return r.walkBytesConstant(k)
	}
	return nil, &UnsupportedExpressionShapeError{Shape: fmt.Sprintf("%T", c)}
}

// stripStringLiteral removes the single-quote delimiters from a
// STRING_LITERAL token's text and unescapes doubled quotes. Used
// by the grammar-Predicate handlers that receive STRING_LITERAL
// tokens directly (LikePredicate pattern) rather than going
// through the ConstantExpressionAtom dispatch.
func stripStringLiteral(text string) string {
	if len(text) >= 2 && text[0] == '\'' && text[len(text)-1] == '\'' {
		return strings.ReplaceAll(text[1:len(text)-1], "''", "'")
	}
	return text
}

// InListNullError signals that a NULL literal was found in an IN list.
// Java rejects these with "NULL values are not allowed in the IN list".
type InListNullError struct{}

func (*InListNullError) Error() string {
	return "NULL values are not allowed in the IN list"
}

// InColumnRefError signals `x IN y` where y is a column reference,
// not an explicit value list. Java rejects this as unsupported syntax.
type InColumnRefError struct{}

func (*InColumnRefError) Error() string {
	return "IN with a column reference is not supported"
}

// UnsupportedExpressionShapeError signals a parse-tree shape the
// seed walker doesn't handle. Callers catching this can fall back
// to the existing logical-builder path (which handles the full
// surface) rather than failing at the SQL level.
type UnsupportedExpressionShapeError struct {
	Shape string
}

func (e *UnsupportedExpressionShapeError) Error() string {
	return fmt.Sprintf("expr.WalkExpression: unsupported shape: %s", e.Shape)
}

// walkBytesConstant handles hex (`x'CAFE'`) and base64 (`b64'yv4='`)
// byte literals. Matches Java's ExpressionVisitor.visitBytesLiteral
// + ParseHelpers.parseBytes: strips the prefix/suffix, decodes the
// body, and wraps the result in a ConstantValue with BYTES type.
//
// Invalid hex (odd length, non-hex chars) or invalid base64 surfaces
// as InvalidBinaryLiteralError so callers can map to SQLSTATE 22F03.
func (r *Resolver) walkBytesConstant(bc *antlrgen.BytesConstantContext) (values.Value, error) {
	bl := bc.BytesLiteral()
	if bl == nil {
		return nil, &InvalidBinaryLiteralError{Text: bc.GetText(), Reason: "empty bytes literal"}
	}
	blc, ok := bl.(*antlrgen.BytesLiteralContext)
	if !ok {
		return nil, &InvalidBinaryLiteralError{Text: bc.GetText(), Reason: fmt.Sprintf("unexpected BytesLiteral ctx %T", bl)}
	}
	if hexLit := blc.HEXADECIMAL_LITERAL(); hexLit != nil {
		text := hexLit.GetText()
		body := stripBytesPrefix(text, "x")
		out, err := hex.DecodeString(body)
		if err != nil {
			return nil, &InvalidBinaryLiteralError{Text: text, Reason: err.Error()}
		}
		return r.ResolveConstant(out)
	}
	if b64Lit := blc.BASE64_LITERAL(); b64Lit != nil {
		text := b64Lit.GetText()
		body := stripBytesPrefix(text, "b64")
		out, err := base64.StdEncoding.Strict().DecodeString(body)
		if err != nil {
			return nil, &InvalidBinaryLiteralError{Text: text, Reason: err.Error()}
		}
		return r.ResolveConstant(out)
	}
	return nil, &InvalidBinaryLiteralError{Text: bc.GetText(), Reason: "bytes literal must be hex or base64"}
}

// stripBytesPrefix removes the `<prefix>'...'` wrapping from a bytes
// literal text token (e.g. `x'CAFE'` → `CAFE`, `b64'yv4='` → `yv4=`).
// Case-insensitive on the prefix.
func stripBytesPrefix(text, prefix string) string {
	lower := strings.ToLower(text)
	if strings.HasPrefix(lower, prefix) {
		text = text[len(prefix):]
	}
	text = strings.TrimPrefix(text, "'")
	text = strings.TrimSuffix(text, "'")
	return text
}

// InvalidBinaryLiteralError signals a malformed hex or base64 byte
// literal. Should be mapped to SQLSTATE 22F03
// INVALID_BINARY_REPRESENTATION by the caller.
type InvalidBinaryLiteralError struct {
	Text   string
	Reason string
}

func (e *InvalidBinaryLiteralError) Error() string {
	return fmt.Sprintf("invalid binary literal %q: %s", e.Text, e.Reason)
}

// walkScalarSubquery handles `(SELECT ...)` scalar subquery atoms.
// Delegates to the Resolver's SubqueryPlanner.BuildScalar to build
// the inner query's logical plan and allocate a fresh alias. Returns
// a ScalarSubqueryValue referencing the alias. Declines with
// UnsupportedExpressionShapeError when no SubqueryPlanner is installed.
func (r *Resolver) walkScalarSubquery(ctx *antlrgen.SubqueryExpressionAtomContext) (values.Value, error) {
	if r.subqueryPlanner == nil {
		return nil, &UnsupportedExpressionShapeError{Shape: "scalar subquery (no SubqueryPlanner)"}
	}
	q := ctx.Query()
	if q == nil {
		return nil, &UnsupportedExpressionShapeError{Shape: "scalar subquery without inner Query"}
	}
	alias, err := r.subqueryPlanner.BuildScalar(q)
	if err != nil {
		return nil, err
	}
	return values.NewScalarSubqueryValue(alias), nil
}

// walkExistsPredicate handles `EXISTS (SELECT ...)`. Delegates to the
// Resolver's SubqueryPlanner to build the inner query's logical plan
// and allocate a fresh existential alias. Returns an ExistsPredicate
// wrapping the alias. Declines with UnsupportedExpressionShapeError
// when no SubqueryPlanner is installed.
func (r *Resolver) walkExistsPredicate(ctx *antlrgen.ExistsExpressionAtomContext) (predicates.QueryPredicate, error) {
	if r.subqueryPlanner == nil {
		return nil, &UnsupportedExpressionShapeError{Shape: "EXISTS (no SubqueryPlanner)"}
	}
	q := ctx.Query()
	if q == nil {
		return nil, &UnsupportedExpressionShapeError{Shape: "EXISTS without inner Query"}
	}
	alias, err := r.subqueryPlanner.BuildExists(q)
	if err != nil {
		return nil, err
	}
	return predicates.NewExistsPredicate(alias), nil
}

// NumericOverflowLiteralError signals that a numeric literal overflows
// its target type (e.g. 1e400 overflows float64). Should be mapped
// to SQLSTATE 22003 NUMERIC_VALUE_OUT_OF_RANGE.
type NumericOverflowLiteralError struct {
	Text string
}

func (e *NumericOverflowLiteralError) Error() string {
	return fmt.Sprintf("numeric literal out of range: %s", e.Text)
}
