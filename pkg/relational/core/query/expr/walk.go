package expr

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"github.com/antlr4-go/antlr/v4"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/api"
	antlrgen "fdb.dev/pkg/relational/core/parser/gen"
	"fdb.dev/pkg/relational/core/query/semantic"
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
	// RFC-141 R4 convergence backstop (P1b). In a projection position
	// (allowComparisons), only a top-level projected EXISTS / NOT-EXISTS — the
	// whole SELECT item, or its single paren/NOT wrapper of a bare EXISTS — folds
	// correctly (the switch arms below place the ExistsValue in the result value
	// where the FlatMap evaluates it with the existential binding live). An EXISTS
	// NESTED inside another expression (`CASE WHEN EXISTS(...) THEN ...`,
	// `EXISTS(...) AND x`, a comparison, arithmetic, ...) would fall through to the
	// predicate path → a predicateValue whose ExistsValue is evaluated ABOVE the
	// FlatMap with the binding dead — constant false / NULL, a silent wrong result.
	// EXISTS can be nested arbitrarily deep, so rather than point-handle each shape
	// (which never converges), structurally DETECT any contained EXISTS that is not
	// the directly-foldable top-level shape and reject the query cleanly.
	if allowComparisons && ContainsExistsAtom(ctx) && !isDirectlyFoldableProjectedExists(ctx) {
		return nil, &NestedExistsProjectionError{}
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
			// RFC-141: a parenthesized EXISTS in a projection — `SELECT (EXISTS(
			// ...))` — surfaces here as a PredicatedExpression over a paren-wrap
			// RecordConstructor. walkAtom's recursion would re-enter via
			// WalkExpression (predicate context, allowComparisons=false) and lose
			// the projection position, yielding a predicateValue → NULL column.
			// Detect the wrapped EXISTS atom and fold it as a Value directly, the
			// same as the bare `SELECT EXISTS(...)` case.
			if existsAtom := existsAtomInExpressionAtom(c.ExpressionAtom()); existsAtom != nil {
				return r.walkExistsValue(existsAtom)
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
		// RFC-141: `NOT EXISTS (...)` in a projection is the boolean negation of
		// the projected ExistsValue — a NotValue over the ExistsValue, so the
		// column carries true/false (ExistsValue.eval reads the existential
		// binding). Only EXISTS surfaces an evaluable Value through the NOT here;
		// other NOT operands stay on the predicate path (their per-row eval is
		// the 3VL predicate, not a column value). Detect the EXISTS atom
		// structurally (no double-walk of non-EXISTS operands).
		if allowComparisons {
			if existsAtom := existsAtomOf(c.Expression()); existsAtom != nil {
				childVal, err := r.walkExistsValue(existsAtom)
				if err != nil {
					return nil, err
				}
				return values.NewNotValue(childVal), nil
			}
		}
		child, err := r.WalkPredicate(c.Expression())
		if err != nil {
			return nil, err
		}
		return &predicateValue{pred: r.ResolveNot(child)}, nil
	case *antlrgen.ExistsExpressionAtomContext:
		// RFC-141: the SAME ExistsValue is produced for EXISTS regardless of
		// position; the consumer decides. A SELECT-element/projection position
		// (WalkExpressionForProjection → allowComparisons) uses the ExistsValue
		// directly as the column value; a predicate position wraps it via
		// ExistsValueToQueryPredicate. The split is structural (which walk is
		// invoked), not a flag on the value — mirroring Java's single-visitor /
		// two-consumer design.
		if allowComparisons {
			return r.walkExistsValue(c)
		}
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
	case *antlrgen.ArrayConstructorExpressionAtomContext:
		// `[1.0, 0.0, 0.0]` — a numeric array / vector literal (the query
		// vector for a K-NN distance function).
		return r.walkArrayConstructor(a.ArrayConstructor())
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
	if udf, ok := fc.(*antlrgen.UserDefinedScalarFunctionCallContext); ok {
		// `CARDINALITY(arr)` and other bare-ID function calls parse as a
		// UserDefinedScalarFunctionCall (the grammar's `ID '(' args ')'`
		// fallthrough). Route recognised by-name built-ins here through the
		// dedicated dispatch; everything else declines so the caller falls
		// back. This is the SINGLE by-name gate Torvalds blessed — not a
		// fourth hand-maintained keyword list.
		return r.walkUserDefinedScalarFunction(udf)
	}
	if nonAgg, ok := fc.(*antlrgen.NonAggregateFunctionCallContext); ok {
		return r.walkNonAggregateWindowedFunction(nonAgg)
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
	return values.NewPickValue(selector, alternatives, caseResultType(alternatives)), nil
}

// caseResultType computes a CASE expression's result type as the common
// supertype (Java's Type.maximumType) of its THEN/ELSE branch values, made
// nullable (a CASE with no matching branch and no ELSE yields NULL). Falls
// back to UnknownType when any branch type is itself unknown or the branches
// cannot be unified — preserving the prior conservative behaviour rather than
// guessing. Without this the result-set column metadata reports UNKNOWN for
// every CASE (Java reports the branch type).
func caseResultType(alternatives []values.Value) values.Type {
	return commonBranchType(alternatives)
}

// commonBranchType computes the common supertype (Java's Type.maximumType) of a
// set of branch/argument values, made nullable. NULL and UNKNOWN branches carry
// no type constraint and are skipped — a `CASE WHEN ... THEN NULL ELSE v END`
// is typed by v, matching Java. Returns UnknownType only when no branch has a
// concrete type or the concrete branches cannot be unified (preserving the
// prior conservative behaviour rather than guessing).
func commonBranchType(branches []values.Value) values.Type {
	types := make([]values.Type, 0, len(branches))
	for _, b := range branches {
		if b == nil {
			continue
		}
		// A literal NULL carries no type constraint — skip it so the concrete
		// branches type the result (`CASE WHEN c THEN NULL ELSE v END` is v's
		// type). NULL is built as NewNullValue(TypeUnknown), so its type code
		// is TypeCodeUnknown — it must be detected by value KIND, not type
		// code, or it gets confused with a genuine unknown (Graefe).
		if _, isNull := b.(*values.NullValue); isNull {
			continue
		}
		t := b.Type()
		if t == nil || t.Code() == values.TypeCodeNull {
			continue
		}
		if t.Code() == values.TypeCodeUnknown {
			// A non-NULL branch of genuinely unknown type (scalar subquery,
			// parameter) could yield any type, so we cannot let the concrete
			// branches dictate the result — keep it unknown (P2).
			return values.UnknownType
		}
		types = append(types, t)
	}
	if len(types) == 0 {
		return values.UnknownType
	}
	mt := values.MaximumTypeOfMany(types...)
	if mt == nil {
		return values.UnknownType
	}
	return values.WithNullability(mt, true)
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
	return values.NewPickValue(selector, alternatives, caseResultType(alternatives)), nil
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

// predicateChildValues returns the operand Values reachable inside a predicate
// (used to surface a predicateValue's hidden value references to value-tree
// walks). Recurses the boolean connectives; a leaf comparison/value predicate
// yields its operand value(s). Unknown predicate types yield nothing (degrading
// to the prior opaque behavior rather than guessing). Mirrors the predicate
// shapes SimplifyPredicateValues handles.
func predicateChildValues(p predicates.QueryPredicate) []values.Value {
	switch q := p.(type) {
	case *predicates.ComparisonPredicate:
		var out []values.Value
		if q.Operand != nil {
			out = append(out, q.Operand)
		}
		if q.Comparison.Operand != nil {
			out = append(out, q.Comparison.Operand)
		}
		return out
	case *predicates.ValuePredicate:
		if q.Value != nil {
			return []values.Value{q.Value}
		}
	case *predicates.ExistentialValuePredicate:
		if q.Value != nil {
			return []values.Value{q.Value}
		}
	case *predicates.AndPredicate:
		var out []values.Value
		for _, sp := range q.SubPredicates {
			out = append(out, predicateChildValues(sp)...)
		}
		return out
	case *predicates.OrPredicate:
		var out []values.Value
		for _, sp := range q.SubPredicates {
			out = append(out, predicateChildValues(sp)...)
		}
		return out
	case *predicates.NotPredicate:
		return predicateChildValues(q.Child)
	}
	return nil
}

// predicateWithChildValues reconstructs a predicate, substituting its operand
// Values with the entries of newCh consumed in the SAME order predicateChildValues
// yields them. The inverse of predicateChildValues — used by
// predicateValue.WithChildrenValue so values.Replace / RebaseValue can rewrite
// (e.g. correlation-rebase) the values inside a CASE WHEN condition. Returns the
// input unchanged on an arity mismatch (defensive; the framework always passes a
// matching slice).
func predicateWithChildValues(p predicates.QueryPredicate, newCh []values.Value) predicates.QueryPredicate {
	idx := 0
	var rebuild func(predicates.QueryPredicate) predicates.QueryPredicate
	next := func() (values.Value, bool) {
		if idx >= len(newCh) {
			return nil, false
		}
		v := newCh[idx]
		idx++
		return v, true
	}
	rebuild = func(p predicates.QueryPredicate) predicates.QueryPredicate {
		switch q := p.(type) {
		case *predicates.ComparisonPredicate:
			op := q.Operand
			if q.Operand != nil {
				if v, ok := next(); ok {
					op = v
				}
			}
			cmp := q.Comparison
			if q.Comparison.Operand != nil {
				if v, ok := next(); ok {
					cmp.Operand = v
				}
			}
			return &predicates.ComparisonPredicate{Operand: op, Comparison: cmp}
		case *predicates.ValuePredicate:
			if q.Value != nil {
				if v, ok := next(); ok {
					return predicates.NewValuePredicate(v)
				}
			}
			return q
		case *predicates.ExistentialValuePredicate:
			if q.Value != nil {
				if v, ok := next(); ok {
					// Use the validated constructor (a rebased existential operand
					// stays a QuantifiedObjectValue). Defensive: this arm is unreached
					// in practice — a predicateValue only ever wraps a CASE WHEN
					// condition, and EXISTS-in-CASE-WHEN is rejected upstream.
					return predicates.MustNewExistentialValuePredicate(v, q.Comparison)
				}
			}
			return q
		case *predicates.AndPredicate:
			subs := make([]predicates.QueryPredicate, len(q.SubPredicates))
			for i, sp := range q.SubPredicates {
				subs[i] = rebuild(sp)
			}
			return predicates.NewAnd(subs...)
		case *predicates.OrPredicate:
			subs := make([]predicates.QueryPredicate, len(q.SubPredicates))
			for i, sp := range q.SubPredicates {
				subs[i] = rebuild(sp)
			}
			return predicates.NewOr(subs...)
		case *predicates.NotPredicate:
			return predicates.NewNot(rebuild(q.Child))
		}
		return p
	}
	if len(newCh) != len(predicateChildValues(p)) {
		return p
	}
	return rebuild(p)
}

// Children exposes the operand Values of the WRAPPED predicate (a CASE WHEN
// condition like `a.x > 5`). Without this, the predicate's value references are
// invisible to every values.WalkValue-based walk — notably GetCorrelatedToOfValue
// and PushFilterBelowJoinRule's predicateSingleSide — so a CASE used as a
// cross-table comparison operand (`CASE WHEN a.x>5 THEN .. END = c.y`) was
// mis-classified as single-side and wrongly pushed below the join, where `a.x` is
// unbound → silent WRONG ROWS. (The matching WithChildrenValue below implements
// values.SelfWithChildren, so values.Replace / RebaseValue now REBUILD the wrapped
// predicate with the substituted children rather than returning the node unchanged;
// equality/hash below use the whole predicate, which is consistent with these
// children.)
func (pv *predicateValue) Children() []values.Value                 { return predicateChildValues(pv.pred) }
func (pv *predicateValue) Name() string                             { return "predicate" }
func (pv *predicateValue) Type() values.Type                        { return values.TypeBool }
func (pv *predicateValue) GetPredicate() predicates.QueryPredicate  { return pv.pred }
func (pv *predicateValue) SetPredicate(p predicates.QueryPredicate) { pv.pred = p }

// WithChildrenValue implements values.SelfWithChildren: it rebuilds the wrapped
// predicate with the substituted operand Values (the inverse of Children() above),
// so values.Replace / RebaseValue can rewrite the values inside the CASE WHEN
// condition (e.g. correlation rebase) instead of hitting withChildren's
// unhandled-type panic.
func (pv *predicateValue) WithChildrenValue(newChildren []values.Value) values.Value {
	return &predicateValue{pred: predicateWithChildValues(pv.pred, newChildren)}
}

// EqualsWithoutChildrenValue implements values.SelfEqualsWithoutChildren so the
// Cascades matcher (values.EqualsWithoutChildren) can structurally compare a
// predicate-as-value without the values package importing expr (which would
// cycle). Its node equality is the FULL structural equality of the wrapped
// predicate. Children() now also exposes the predicate's operand values, so the
// matcher additionally recurses them — a redundant (but consistent) double-check,
// since equal-whole-predicate implies equal-operands. Non-alias-aware, matching
// EqualsWithoutChildren's structural (not semantic) contract.
//
// NOTE: the SelfEqualsWithoutChildren interface carries no alias map, so the
// node-level comparison is alias-blind. AND-ed with the alias-AWARE child
// recursion this makes predicateValue equality effectively alias-blind (stricter
// → can only MISS interning, never produce a false merge). That is safe today because the
// memo interns non-merge selects under the identity alias map (so raw correlation
// equality is exactly the alias-aware result). If alias-aware interning is ever
// widened to selects whose result trees carry predicateValues, this interface (and
// SelfSemanticHash below) will need an alias-map parameter to avoid a false memo
// collapse — see DIVERGENCES / the value-layer alias-threading work.
func (pv *predicateValue) EqualsWithoutChildrenValue(other values.Value) bool {
	o, ok := other.(*predicateValue)
	if !ok {
		return false
	}
	return predicates.StructurallyEqual(pv.pred, o.pred)
}

// SemanticHashDiscriminator implements values.SelfSemanticHash so the memo hash
// folds the wrapped predicate — otherwise every predicateValue shares the bare
// "v:predicate" bucket and predicate-in-projection / CASE-heavy SQL degrades memo
// lookup. predicates.StructuralHash is the hash analog of the StructurallyEqual
// used by EqualsWithoutChildrenValue above, so equal predicateValues hash equal
// (the equal⟹same-hash invariant the memo requires).
func (pv *predicateValue) SemanticHashDiscriminator() uint64 {
	return predicates.StructuralHash(pv.pred)
}

func (pv *predicateValue) Evaluate(evalCtx any) (any, error) {
	if pv.pred == nil {
		return nil, nil
	}
	res, err := pv.pred.Eval(evalCtx)
	if err != nil {
		return nil, err
	}
	switch res {
	case predicates.TriTrue:
		return true, nil
	case predicates.TriFalse:
		return false, nil
	default:
		return nil, nil
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
	// Vector distance functions (euclidean_distance, cosine_distance, ...)
	// build a DistanceValue — the K-NN ORDER BY operand inside an OVER clause.
	// Java models these as DistanceValue (an ArithmeticValue subclass).
	if op, isDistance := distanceOperatorForFunc(name); isDistance {
		if len(args) != 2 {
			return nil, &UnsupportedExpressionShapeError{
				Shape: fmt.Sprintf("%s requires exactly 2 arguments, got %d", name, len(args)),
			}
		}
		return values.NewDistanceValue(op, args[0], args[1]), nil
	}
	typ, ok := scalarFunctionResultType(name)
	if !ok {
		return nil, &UnsupportedExpressionShapeError{Shape: fmt.Sprintf("scalar function %q (not in seed catalogue)", name)}
	}
	// Polymorphic value-preserving functions (COALESCE, GREATEST, LEAST, ...)
	// carry UnknownType by name; infer the concrete result type from the
	// argument types so result-set metadata reports the real type (Java does).
	if typ == nil || typ.Code() == values.TypeCodeUnknown {
		if inferred := polymorphicResultType(name, args); inferred != nil {
			typ = inferred
		}
	}
	return values.NewScalarFunctionValue(name, typ, args...), nil
}

// walkUserDefinedScalarFunction handles `name '(' args ')'` calls whose
// name is a bare ID (the grammar's UserDefinedScalarFunctionCall). The
// only such call wired today is the CARDINALITY built-in: it parses here
// rather than as a ScalarFunctionCall because CARDINALITY has no grammar
// token (it is not in the `scalarFunctionName` keyword set). This is the
// single by-name built-in dispatch gate (Torvalds' option (b)) — a
// recognised name builds its dedicated Value; everything else declines so
// the caller falls back. A quoted name (`"cardinality"`) is a deliberate
// user-defined reference, not the built-in, so only the unquoted ID form
// is matched.
func (r *Resolver) walkUserDefinedScalarFunction(udf *antlrgen.UserDefinedScalarFunctionCallContext) (values.Value, error) {
	if udf == nil {
		return nil, fmt.Errorf("expr.walkUserDefinedScalarFunction: nil")
	}
	nameCtx := udf.UserDefinedScalarFunctionName()
	if nameCtx == nil {
		return nil, &UnsupportedExpressionShapeError{Shape: "UserDefinedScalarFunctionCall without name"}
	}
	nameNode, ok := nameCtx.(*antlrgen.UserDefinedScalarFunctionNameContext)
	if !ok || nameNode.ID() == nil {
		// Quoted (DOUBLE_QUOTE_ID) or otherwise non-bare — not a built-in.
		return nil, &UnsupportedExpressionShapeError{Shape: fmt.Sprintf("user-defined scalar function %q", nameCtx.GetText())}
	}
	name := strings.ToUpper(nameNode.ID().GetText())
	switch name {
	case "CARDINALITY":
		return r.walkCardinality(udf.FunctionArgs())
	}
	return nil, &UnsupportedExpressionShapeError{Shape: fmt.Sprintf("user-defined scalar function %q (not a built-in)", name)}
}

// walkCardinality builds the dedicated CardinalityValue for
// `CARDINALITY(arr)`. Mirrors Java's CardinalityFn.encapsulateInternal:
// arity exactly 1, and the argument must be array-typed (Java's ctor
// asserts childValue.getResultType().isArray() → INCOMPATIBLE_TYPE). The
// array-type check lives here — the earliest point with the resolved
// argument Type and access to the SQLSTATE error codes — and raises
// CANNOT_CONVERT_TYPE (22000) for a non-array argument, matching the
// arrays-cardinality.yamsql expectation (SELECT CARDINALITY("id") /
// CARDINALITY(1) → CANNOT_CONVERT_TYPE). The result is a dedicated
// CardinalityValue, NOT a generic ScalarFunctionValue: CARDINALITY needs
// its own nullable-INT typing and array validation.
func (r *Resolver) walkCardinality(fa antlrgen.IFunctionArgsContext) (values.Value, error) {
	args, err := r.walkFunctionArgs(fa)
	if err != nil {
		return nil, err
	}
	if len(args) != 1 {
		return nil, api.NewErrorf(api.ErrCodeCannotConvertType,
			"CARDINALITY takes exactly one argument, got %d", len(args))
	}
	arg := args[0]
	if arg == nil || !values.IsArray(arg.Type()) {
		// Java: SemanticException.check(isArray(), INCOMPATIBLE_TYPE,
		// "The argument of CARDINALITY() must be an array expression.").
		// The SQL layer surfaces that as CANNOT_CONVERT_TYPE (22000).
		return nil, api.NewErrorf(api.ErrCodeCannotConvertType,
			"the argument of CARDINALITY() must be an array expression")
	}
	return values.NewCardinalityValue(arg), nil
}

// walkFunctionArgs walks a FunctionArgs context into the resolved
// argument Value list, recursing each arg through WalkExpression so
// nested expressions compose. Shared by the by-name built-in dispatch.
func (r *Resolver) walkFunctionArgs(fa antlrgen.IFunctionArgsContext) ([]values.Value, error) {
	args := []values.Value{}
	if fa == nil {
		return args, nil
	}
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
	return args, nil
}

// polymorphicResultType infers the result type of a value-preserving
// polymorphic scalar function from its argument types, mirroring Java. Returns
// nil (keep UnknownType) when the type can't be determined from concrete args.
func polymorphicResultType(name string, args []values.Value) values.Type {
	concrete := func(t values.Type) values.Type {
		if t == nil || t.Code() == values.TypeCodeUnknown {
			return nil
		}
		return t
	}
	switch name {
	case "COALESCE", "IFNULL", "GREATEST", "LEAST":
		// Result is the common supertype of all branches.
		return concrete(commonBranchType(args))
	case "IF", "IIF":
		// IF(cond, then, else): common supertype of the value branches.
		if len(args) >= 2 {
			return concrete(commonBranchType(args[1:]))
		}
	case "NULLIF":
		// NULLIF(a, b) is a (possibly NULL) so it has a's type.
		if len(args) >= 1 && args[0] != nil {
			if t := concrete(args[0].Type()); t != nil {
				return values.WithNullability(t, true)
			}
		}
	case "MOD":
		// MOD(a, b) promotes both operands (MOD(id, 2.5) yields a DOUBLE at
		// runtime), same as arithmetic — P2.
		return concrete(commonBranchType(args))
	case "ABS", "FLOOR", "CEIL", "CEILING", "ROUND", "SIGN":
		// Numeric, type-preserving in the first operand.
		if len(args) >= 1 && args[0] != nil {
			return concrete(args[0].Type())
		}
	}
	return nil
}

// distanceOperatorForFunc maps a SQL distance-function name to its
// DistanceOperator. Returns (_, false) for non-distance functions.
func distanceOperatorForFunc(name string) (values.DistanceOperator, bool) {
	switch name {
	case "EUCLIDEAN_DISTANCE":
		return values.DistanceEuclidean, true
	case "EUCLIDEAN_SQUARE_DISTANCE":
		return values.DistanceEuclideanSquare, true
	case "COSINE_DISTANCE":
		return values.DistanceCosine, true
	case "DOT_PRODUCT_DISTANCE":
		return values.DistanceDotProduct, true
	default:
		return 0, false
	}
}

// walkNonAggregateWindowedFunction builds a RowNumberValue from
//
//	ROW_NUMBER() OVER (PARTITION BY ... ORDER BY <distance>(field, q) [OPTIONS ef_search = N])
//
// Mirrors Java's ExpressionVisitor.visitNonAggregateWindowedFunction /
// visitOverClause. Only ROW_NUMBER is wired: Java implements the vector
// K-NN window function exclusively via ROW_NUMBER (RANK is index-only via a
// rank index; LAG/LEAD have no value class). The ORDER BY expression becomes
// the row-number argument (the distance value); PARTITION BY columns become
// the partitioning values; OPTIONS ef_search threads the HNSW search knob.
func (r *Resolver) walkNonAggregateWindowedFunction(fc *antlrgen.NonAggregateFunctionCallContext) (values.Value, error) {
	wfc, ok := fc.NonAggregateWindowedFunction().(*antlrgen.NonAggregateWindowedFunctionContext)
	if !ok || wfc == nil {
		return nil, &UnsupportedExpressionShapeError{Shape: "malformed non-aggregate windowed function"}
	}
	if wfc.ROW_NUMBER() == nil {
		fn := ""
		if wfc.GetFunctionName() != nil {
			fn = wfc.GetFunctionName().GetText()
		}
		return nil, &UnsupportedExpressionShapeError{
			Shape: fmt.Sprintf("window function %q (only ROW_NUMBER is supported)", fn),
		}
	}
	overc, ok := wfc.OverClause().(*antlrgen.OverClauseContext)
	if !ok || overc == nil {
		return nil, &UnsupportedExpressionShapeError{Shape: "ROW_NUMBER requires an OVER clause"}
	}
	if overc.WindowName() != nil {
		return nil, &UnsupportedExpressionShapeError{Shape: "named window functions not supported"}
	}
	specc, ok := overc.WindowSpec().(*antlrgen.WindowSpecContext)
	if !ok || specc == nil {
		return nil, &UnsupportedExpressionShapeError{Shape: "malformed window specification"}
	}

	// PARTITION BY columns → partitioning values.
	var partitions []values.Value
	if pc, ok := specc.PartitionClause().(*antlrgen.PartitionClauseContext); ok && pc != nil {
		for _, fid := range pc.AllFullId() {
			pv, err := r.walkColumnRef(fid)
			if err != nil {
				return nil, err
			}
			partitions = append(partitions, pv)
		}
	}

	// ORDER BY expressions → the row-number argument values (the distance
	// expression for K-NN). Java requires ascending (or unspecified) sort.
	var args []values.Value
	if obc, ok := specc.OrderByClause().(*antlrgen.OrderByClauseContext); ok && obc != nil {
		for _, obe := range obc.AllOrderByExpression() {
			obec, ok := obe.(*antlrgen.OrderByExpressionContext)
			if !ok {
				return nil, &UnsupportedExpressionShapeError{Shape: "malformed ORDER BY expression in OVER clause"}
			}
			if occ, ok := obec.OrderClause().(*antlrgen.OrderClauseContext); ok && occ != nil && occ.DESC() != nil {
				return nil, &UnsupportedExpressionShapeError{
					Shape: "window function ORDER BY must be ascending (DESC not supported)",
				}
			}
			av, err := r.WalkExpression(obec.Expression())
			if err != nil {
				return nil, err
			}
			args = append(args, av)
		}
	}

	// OPTIONS ef_search = N (HNSW search-quality knob).
	var efSearch *int
	if woc, ok := specc.WindowOptionsClause().(*antlrgen.WindowOptionsClauseContext); ok && woc != nil {
		for _, opt := range woc.AllWindowOption() {
			optc, ok := opt.(*antlrgen.WindowOptionContext)
			if !ok || optc.EF_SEARCH() == nil || optc.GetEfSearch() == nil {
				continue
			}
			n, err := strconv.Atoi(optc.GetEfSearch().GetText())
			if err != nil {
				return nil, &UnsupportedExpressionShapeError{
					Shape: fmt.Sprintf("invalid ef_search value %q", optc.GetEfSearch().GetText()),
				}
			}
			efSearch = &n
		}
	}

	return values.NewRowNumberValue(partitions, args, efSearch, nil), nil
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

// walkArrayConstructor builds a numeric array / vector literal `[a, b, c]`
// into a []float64 LiteralValue — the query-vector operand of a K-NN distance
// function. Elements must be numeric literals (the common vector-search shape).
func (r *Resolver) walkArrayConstructor(ac antlrgen.IArrayConstructorContext) (values.Value, error) {
	acc, ok := ac.(*antlrgen.ArrayConstructorContext)
	if !ok || acc == nil {
		return nil, &UnsupportedExpressionShapeError{Shape: "malformed array constructor"}
	}
	exprsCtx := acc.Expressions()
	if exprsCtx == nil {
		return values.LiteralValue([]float64{}), nil
	}
	ec, ok := exprsCtx.(*antlrgen.ExpressionsContext)
	if !ok {
		return nil, &UnsupportedExpressionShapeError{Shape: "malformed array elements"}
	}
	elems := ec.AllExpression()
	vec := make([]float64, 0, len(elems))
	for _, e := range elems {
		// Walk each element through the resolver instead of string-parsing
		// GetText(): this resolves negative literals (NegativeDecimalConstant)
		// and integer literals (promoted to float64) via the typed parse tree,
		// and rejects non-constant expressions cleanly. (GetText() is used only
		// for the human-readable error message, never to read the value.)
		v, err := r.WalkExpression(e)
		if err != nil {
			return nil, err
		}
		f, ok := numericConstantToFloat64(v)
		if !ok {
			return nil, &UnsupportedExpressionShapeError{
				Shape: fmt.Sprintf("array literal element %q is not a numeric constant", e.GetText()),
			}
		}
		vec = append(vec, f)
	}
	return values.LiteralValue(vec), nil
}

// numericConstantToFloat64 extracts a float64 from a resolved constant Value.
// Returns ok=false for non-constant or non-numeric Values (a column reference,
// a string literal, an arithmetic expression). int/float widths are promoted to
// float64 so integer-literal vector elements (`[1, 0, 0]`) are accepted.
func numericConstantToFloat64(v values.Value) (float64, bool) {
	cv, ok := v.(*values.ConstantValue)
	if !ok {
		return 0, false
	}
	switch n := cv.Value.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int64:
		return float64(n), true
	case int32:
		return float64(n), true
	case int:
		return float64(n), true
	default:
		return 0, false
	}
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
	// Lift a bare value used as a predicate, mirroring Java's
	// Expression.Utils.toUnderlyingPredicate (Expression.java:384-399)
	// branch order. Shared by WHERE and ON (both reach here).
	//
	// 1. Concrete boolean literal TRUE/FALSE → ConstantPredicate (what
	//    the simplifier's constant-fold rules expect; a wrapped boolean
	//    would be opaque and unsimplified).
	if bv, ok := v.(*values.BooleanValue); ok && bv.Value != nil {
		if *bv.Value {
			return predicates.NewConstantPredicate(predicates.TriTrue), nil
		}
		return predicates.NewConstantPredicate(predicates.TriFalse), nil
	}
	// 2. NULL → unknown constant (Java :384, `value instanceof NullValue`).
	//    Detected by VALUE type, not Type().Code(): a NULL literal is built as
	//    NewNullValue(TypeUnknown), so its type code is Unknown — only the
	//    value-type assertion identifies it. Must be folded HERE: the
	//    comparison-form lift below bypasses ValuePredicateConstantFoldRule
	//    (which matches only *ValuePredicate) that `WHERE NULL` relied on.
	if _, isNull := v.(*values.NullValue); isNull {
		return predicates.NewConstantPredicate(predicates.TriUnknown), nil
	}
	switch v.Type().Code() {
	case values.TypeCodeBoolean, values.TypeCodeUnknown:
		// 4. A boolean value (column / expression) → `value = TRUE` (Java
		//    :399). This is byte-for-byte the ComparisonPredicate that
		//    `value = TRUE` builds (ResolveComparison, expr.go:314), so a bare
		//    `WHERE flag` and `WHERE flag = TRUE` unify — same plan, same
		//    EXPLAIN, same semantic hash, and the SAME index match (Go's index
		//    matcher binds only *ComparisonPredicate; a bare ValuePredicate
		//    would never use a boolean index).
		//
		//    UNKNOWN is permitted (permissive-only divergence from Java's strict
		//    BOOLEAN assert): a value Go's pre-plan resolution can't type — a
		//    parameter, an expression, or a CTE/derived column whose projected
		//    type isn't propagated to the outer scope (it may legitimately be
		//    boolean, e.g. `WITH c AS (SELECT NOT flag AS x ...) ... WHERE x`).
		//    Definitively-typed columns (DOUBLE/FLOAT/BYTES) are typed by
		//    sqlTypeToCascadesType and hit the non-boolean branch below — they
		//    are NOT Unknown, so this never silently accepts a known non-boolean.
		return predicates.NewComparisonPredicate(v, predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: values.NewBooleanValue(true),
		}), nil
	default:
		// 3. A definitively-typed non-boolean bare value is a type error
		//    (Java :389 asserts getTypeCode() == BOOLEAN → DATATYPE_MISMATCH).
		//    Clause-agnostic wording: this lift is shared by WHERE and ON.
		return nil, api.NewErrorf(api.ErrCodeDatatypeMismatch,
			"expected boolean expression, got type %s", v.Type())
	}
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

// NestedExistsProjectionError signals a SELECT-list item that CONTAINS an EXISTS
// subquery atom but is NOT a directly-foldable projected EXISTS — i.e. the EXISTS
// is nested inside another expression (`CASE WHEN EXISTS(...) THEN ...`,
// `EXISTS(...) AND x`, a comparison, an arithmetic, ...) rather than being the
// whole SELECT item or its single (paren/NOT) wrapper.
//
// Unlike UnsupportedExpressionShapeError (which the projection callers SWALLOW
// to fall back to the logical-builder text path), this error MUST reject the
// query: the text-fallback path would route the nested EXISTS through the
// predicate walk, registering the existential subquery but evaluating the
// ExistsValue ABOVE the FlatMap with the binding dead — constant false / NULL,
// a silent wrong result. The callers (logical_predicate.go, plan_visitor.go)
// convert it to ErrCodeUnsupportedQuery (RFC-141 R4 convergence
// backstop, P1b).
type NestedExistsProjectionError struct{}

func (e *NestedExistsProjectionError) Error() string {
	return "projected EXISTS in this query shape is not yet supported"
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
// and allocate a fresh existential alias. Returns an
// ExistentialValuePredicate over a QuantifiedObjectValue of the alias
// (RFC-141: the single EXISTS representation, built via the ExistsValue
// → toQueryPredicate bridge). Declines with UnsupportedExpressionShapeError
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
	return predicates.ExistsValueToQueryPredicate(values.NewExistsValue(alias)), nil
}

// walkExistsValue handles `EXISTS (SELECT ...)` in a SELECT-element /
// projection position (RFC-141 Phase 2). It builds the SAME ExistsValue as
// walkExistsPredicate, but returns it directly as a column Value rather than
// bridging to an ExistentialValuePredicate — the consumer (projection) places
// the boolean in the result record; ExistsValue.eval reads the existential
// binding the FlatMap establishes (bound non-null ⇒ true, NULL ⇒ false). The
// existential subquery is registered via the SAME BuildExists call, so the
// translator attaches the existential quantifier identically to the WHERE case
// (the projected-alias registration step collects it into the subquery list).
func (r *Resolver) walkExistsValue(ctx *antlrgen.ExistsExpressionAtomContext) (values.Value, error) {
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
	return values.NewExistsValue(alias), nil
}

// existsAtomOf returns the ExistsExpressionAtomContext if ctx is (or wraps) an
// EXISTS atom, else nil. Used to detect `NOT EXISTS (...)` structurally in a
// projection so it lowers to NotValue(ExistsValue) without speculatively walking
// non-EXISTS operands.
//
// Three parse shapes carry an EXISTS atom (verified against the grammar):
//
//	NOT EXISTS(q)       NotExpression -> ExistsExpressionAtom            (direct)
//	NOT (EXISTS(q))     NotExpression -> PredicatedExpression
//	                      -> RecordConstructorExpressionAtom            (paren-wrap)
//	                        -> RecordConstructor -> ExpressionWithOptionalName
//	                          -> ExistsExpressionAtom
//	NOT ((EXISTS(q)))   same, nested one more paren-wrap (handled by recursion)
//
// The parenthesized form surfaced as a PredicatedExpression
// over a single-element unnamed RecordConstructor — the same paren-wrap shape
// walkRecordConstructor unwraps — so existsAtomOf must descend through it to
// find the ExistsExpressionAtom; otherwise NOT (EXISTS(...)) falls to the
// predicate path and projects NULL.
func existsAtomOf(ctx antlrgen.IExpressionContext) *antlrgen.ExistsExpressionAtomContext {
	switch c := ctx.(type) {
	case *antlrgen.ExistsExpressionAtomContext:
		return c
	case *antlrgen.PredicatedExpressionContext:
		// A grammar Predicate (IS NULL, BETWEEN, ...) attached makes this a real
		// predicate, not a transparent paren-wrap — not an EXISTS atom.
		if c.Predicate() != nil {
			return nil
		}
		return existsAtomInExpressionAtom(c.ExpressionAtom())
	}
	return nil
}

// existsAtomInExpressionAtom finds an ExistsExpressionAtom wrapped in a
// single-element unnamed parenthesized RecordConstructor (`(EXISTS(...))`).
// Returns nil for any other atom shape. Note: EXISTS is an alternative of the
// `expression` rule (not `expressionAtom`), so a bare EXISTS never arrives as an
// IExpressionAtomContext — only the paren-wrap RecordConstructor does, whose
// inner Expression() is the IExpressionContext routed back through existsAtomOf.
func existsAtomInExpressionAtom(atom antlrgen.IExpressionAtomContext) *antlrgen.ExistsExpressionAtomContext {
	rc, ok := atom.(*antlrgen.RecordConstructorExpressionAtomContext)
	if !ok {
		return nil
	}
	rcc, ok := rc.RecordConstructor().(*antlrgen.RecordConstructorContext)
	if !ok || rcc.OfTypeClause() != nil {
		return nil
	}
	exprs := rcc.AllExpressionWithOptionalName()
	if len(exprs) != 1 {
		return nil
	}
	ewon, ok := exprs[0].(*antlrgen.ExpressionWithOptionalNameContext)
	if !ok || ewon.Uid() != nil {
		// A named field is a real record constructor, not a paren-wrap.
		return nil
	}
	// Recurse: handles nested parens `((EXISTS(...)))` and the inner
	// PredicatedExpression wrapper around the EXISTS atom.
	return existsAtomOf(ewon.Expression())
}

// isDirectlyFoldableProjectedExists reports whether a SELECT-list expression is
// one of the directly-foldable projected-EXISTS shapes — the cases the
// walkExpressionInner switch lowers to an evaluable ExistsValue (or its NOT)
// placed in the projection's result value, where the FlatMap evaluates it with
// the existential binding live. Exactly three shapes (mirroring the switch arms):
//
//   - a bare top-level `EXISTS(...)` — an *ExistsExpressionAtomContext;
//   - a top-level `NOT EXISTS(...)` / `NOT (EXISTS(...))` — a *NotExpressionContext
//     whose operand resolves to an EXISTS atom (existsAtomOf);
//   - a parenthesized `(EXISTS(...))` — a *PredicatedExpressionContext (no grammar
//     Predicate attached) whose ExpressionAtom wraps an EXISTS atom
//     (existsAtomInExpressionAtom).
//
// Any OTHER expression that merely CONTAINS an EXISTS somewhere (a CASE, an
// AND/OR, a comparison, an arithmetic) is NOT directly foldable; the
// backstop rejects it. Used only in projection position.
func isDirectlyFoldableProjectedExists(ctx antlrgen.IExpressionContext) bool {
	switch c := ctx.(type) {
	case *antlrgen.ExistsExpressionAtomContext:
		return true
	case *antlrgen.NotExpressionContext:
		return existsAtomOf(c.Expression()) != nil
	case *antlrgen.PredicatedExpressionContext:
		if c.Predicate() != nil {
			return false
		}
		return existsAtomInExpressionAtom(c.ExpressionAtom()) != nil
	}
	return false
}

// introducesNestedQueryScope reports whether tree is a parse-tree node that opens
// a NEW query scope nested inside an expression — a SCALAR subquery `(subquery)`
// used as a value (SubqueryExpressionAtomContext) or an `x IN (subquery)` body
// (InListContext with a QueryExpressionBody). Each such subquery is translated in
// its OWN context, so its WHERE / projection / ORDER BY EXISTS atoms belong to
// that subquery, not the enclosing expression. The structural EXISTS detectors
// below treat these as BOUNDARIES and do not descend into them — otherwise an
// EXISTS that is the nested subquery's own concern would be mis-attributed to the
// outer expression and falsely rejected (RFC-141 R4).
//
// An ExistsExpressionAtomContext (`EXISTS (subquery)`) ALSO opens a query scope,
// but it is the thing the detectors WANT to match at the current level — so it is
// NOT a boundary here. Callers match it explicitly before descent; matching it
// returns true without recursing, so its own subquery is likewise not descended.
func introducesNestedQueryScope(tree antlr.Tree) bool {
	switch tree.(type) {
	case *antlrgen.SubqueryExpressionAtomContext:
		return true
	case *antlrgen.InListContext:
		// `x IN (SELECT ...)`: the InList wraps a QueryExpressionBody. (An
		// `x IN (a, b, c)` value list has no QueryExpressionBody and carries no
		// nested query scope — but it also carries no EXISTS atom, so treating it
		// as a boundary is harmless. We only need the IN-subquery form stopped.)
		return true
	}
	return false
}

// ContainsExistsAtom reports whether the parse tree rooted at ctx contains an
// EXISTS subquery atom at the CURRENT query level — a structural (typed-node)
// walk, no GetText / text matching. Used by the logical builder to reject query
// shapes (e.g. GROUP BY on an EXISTS expression) where the projected/grouped
// EXISTS cannot be folded and would otherwise be silently dropped (RFC-141 §8
// safety guard).
//
// The walk STOPS at nested-subquery boundaries (introducesNestedQueryScope): an
// EXISTS belonging to a nested scalar / IN subquery's OWN clause is that
// subquery's concern (classified in its own translation context), not the outer
// expression's — descending into it would mis-attribute the inner EXISTS to the
// outer level and falsely reject a query the outer level handles fine (RFC-141 R4
// RFC-141 R4). The EXISTS atom itself is matched before descent, so its own
// subquery is correctly never descended either.
func ContainsExistsAtom(ctx antlr.Tree) bool {
	if ctx == nil {
		return false
	}
	// Match an EXISTS atom at the current level (before any boundary check) — this
	// is the node we want; do not descend into its own subquery.
	if _, ok := ctx.(*antlrgen.ExistsExpressionAtomContext); ok {
		return true
	}
	// A nested subquery boundary: its EXISTS atoms are classified in its own
	// translation context — do not descend.
	if introducesNestedQueryScope(ctx) {
		return false
	}
	for i := 0; i < ctx.GetChildCount(); i++ {
		if ContainsExistsAtom(ctx.GetChild(i)) {
			return true
		}
	}
	return false
}

// ContainsSubqueryAtom reports whether the parse tree rooted at ctx contains a
// SCALAR subquery atom `(SELECT ...)` (SubqueryExpressionAtomContext) or an
// `x IN (SELECT ...)` subquery body (an InListContext carrying a
// QueryExpressionBody) at the CURRENT query level. A structural typed-node walk,
// no GetText.
//
// Used by the logical builder to reject these subquery shapes cleanly in
// positions that do not install a SubqueryPlanner — notably a JOIN ON clause,
// where Go (like Java) does not support IN-subqueries or correlated scalar
// subqueries. Without this, the ON resolver's WalkPredicate declines the shape
// with UnsupportedExpressionShapeError and the caller silently DROPS the whole
// ON predicate → the join degrades to a CROSS PRODUCT (silent wrong rows). EXISTS
// atoms are matched separately by ContainsExistsAtom (EXISTS-in-ON IS supported).
//
// An `x IN (a, b, c)` value list (no QueryExpressionBody) is NOT a subquery and
// is not matched; the walk descends into its elements in case one is itself a
// scalar subquery. A matched subquery atom is not descended into — its inner
// SELECT is that subquery's own concern.
func ContainsSubqueryAtom(ctx antlr.Tree) bool {
	if ctx == nil {
		return false
	}
	switch c := ctx.(type) {
	case *antlrgen.SubqueryExpressionAtomContext:
		return true
	case *antlrgen.InListContext:
		if c.QueryExpressionBody() != nil {
			return true
		}
	}
	for i := 0; i < ctx.GetChildCount(); i++ {
		if ContainsSubqueryAtom(ctx.GetChild(i)) {
			return true
		}
	}
	return false
}

// WhereExistsInScalarPosition reports whether the WHERE expression rooted at ctx
// contains an EXISTS atom that is NOT in a directly-handled top-level boolean
// position — i.e. it is buried inside a SCALAR expression (a CASE, a comparison
// operand, an arithmetic, a function-call argument). Such an EXISTS lowers to a
// scalar Value with no existential quantifier driving it, so it evaluates to a
// constant false → a silent wrong result. The WHERE companion to
// isDirectlyFoldableProjectedExists (RFC-141 R4).
//
// An EXISTS is directly-handled in WHERE iff the path from the WHERE root to the
// atom traverses ONLY boolean-combinator nodes: AND/OR (LogicalExpression), NOT
// (NotExpression), and a transparent paren-wrap (a PredicatedExpression with no
// grammar Predicate over a single-element RecordConstructor, or its
// ExpressionAtom). The descent below mirrors that whitelist: it recurses through
// those nodes, treating a reached EXISTS atom (directly, or via existsAtomOf for
// the NOT/paren shapes) as handled. The moment a node that is NOT a boolean
// combinator still CONTAINS an EXISTS atom beneath it, that EXISTS is buried in
// scalar position → return true.
func WhereExistsInScalarPosition(ctx antlrgen.IExpressionContext) bool {
	if ctx == nil {
		return false
	}
	// A directly-handled top-level EXISTS / NOT-EXISTS / paren-(NOT-)EXISTS: the
	// whole node IS the existential boolean term — nothing buried here.
	if existsAtomOf(ctx) != nil {
		return false
	}
	switch c := ctx.(type) {
	case *antlrgen.LogicalExpressionContext:
		// AND / OR: each operand is itself a boolean term — recurse.
		for _, e := range c.AllExpression() {
			if WhereExistsInScalarPosition(e) {
				return true
			}
		}
		return false
	case *antlrgen.NotExpressionContext:
		// NOT(<expr>): the operand is a boolean term — recurse. (existsAtomOf
		// above already handled NOT directly over a bare/paren EXISTS.)
		return WhereExistsInScalarPosition(c.Expression())
	case *antlrgen.PredicatedExpressionContext:
		// A transparent paren-wrap (no grammar Predicate) over a single-element
		// RecordConstructor is a boolean pass-through — recurse into the wrapped
		// expression. Any other PredicatedExpression (a real grammar Predicate:
		// comparison, IN, BETWEEN, IS — i.e. a SCALAR/relational context) that
		// nonetheless contains an EXISTS atom is buried.
		if c.Predicate() == nil {
			if inner := unwrapParenExpression(c.ExpressionAtom()); inner != nil {
				return WhereExistsInScalarPosition(inner)
			}
		}
		return ContainsExistsAtom(ctx)
	}
	// Any other expression node (comparison, arithmetic, CASE, function call, …)
	// is a scalar context — if it contains an EXISTS atom, that EXISTS is buried.
	return ContainsExistsAtom(ctx)
}

// AnyWhereExistsInScalarPosition reports whether the parse subtree rooted at tree
// contains ANY WHERE clause whose expression has an EXISTS atom buried in a scalar
// position (per WhereExistsInScalarPosition). Used to guard statement forms whose
// WHERE is nested and not directly accessible — notably `INSERT … SELECT … WHERE
// CASE WHEN EXISTS(...) …`, whose SELECT-body WHERE is rebuilt through a path that
// bypasses the per-statement WHERE guard. A structural typed-node walk
// (WhereExprContext), no GetText. (RFC-141 R4.)
func AnyWhereExistsInScalarPosition(tree antlr.Tree) bool {
	if tree == nil {
		return false
	}
	if we, ok := tree.(*antlrgen.WhereExprContext); ok {
		if WhereExistsInScalarPosition(we.Expression()) {
			return true
		}
	}
	for i := 0; i < tree.GetChildCount(); i++ {
		child := tree.GetChild(i)
		// Stop at nested-subquery boundaries: a scalar / IN subquery nested in an
		// expression has its OWN WHERE, classified in its own translation context.
		// Descending into it would mis-attribute that subquery's buried-scalar
		// EXISTS to the INSERT's SELECT body and falsely reject (RFC-141 R4).
		// The nested subquery's WHERE is guarded when it plans in its
		// own context. (The INSERT's own SELECT-body WHERE — a WhereExprContext
		// child of the SELECT, not nested under a SubqueryExpressionAtom — is still
		// reached, since the SELECT body itself is not a nested-query-scope node.)
		if introducesNestedQueryScope(child) {
			continue
		}
		if AnyWhereExistsInScalarPosition(child) {
			return true
		}
	}
	return false
}

// unwrapParenExpression returns the inner IExpressionContext of a single-element
// unnamed parenthesized RecordConstructor `(<expr>)`, or nil for any other atom
// shape. The boolean-context analog of existsAtomInExpressionAtom's unwrap,
// reused by WhereExistsInScalarPosition to descend through `( ... )` wrappers.
func unwrapParenExpression(atom antlrgen.IExpressionAtomContext) antlrgen.IExpressionContext {
	rc, ok := atom.(*antlrgen.RecordConstructorExpressionAtomContext)
	if !ok {
		return nil
	}
	rcc, ok := rc.RecordConstructor().(*antlrgen.RecordConstructorContext)
	if !ok || rcc.OfTypeClause() != nil {
		return nil
	}
	exprs := rcc.AllExpressionWithOptionalName()
	if len(exprs) != 1 {
		return nil
	}
	ewon, ok := exprs[0].(*antlrgen.ExpressionWithOptionalNameContext)
	if !ok || ewon.Uid() != nil {
		return nil
	}
	return ewon.Expression()
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
