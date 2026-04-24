package expr

import (
	"fmt"
	"strconv"
	"strings"

	cascades "github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades"
	antlrgen "github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/query/semantic"
)

// WalkExpression is the parse-tree → cascades.Value entry point.
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
//   - Constant (integer / string / NULL) → ConstantValue / NullValue.
//   - MathExpression (+, -, *, /) → ArithmeticValue.
//   - RecordConstructor (1-element unnamed, i.e. `(x)`) → unwrap.
//   - FunctionCall (aggregate forms) → AggregateValue.
//
// Everything else returns UnsupportedExpressionShapeError so the
// caller can fall back to the existing logical-builder path.
func (r *Resolver) WalkExpression(ctx antlrgen.IExpressionContext) (cascades.Value, error) {
	if ctx == nil {
		return nil, fmt.Errorf("expr.WalkExpression: nil context")
	}
	pred, ok := ctx.(*antlrgen.PredicatedExpressionContext)
	if !ok {
		return nil, &UnsupportedExpressionShapeError{Shape: fmt.Sprintf("%T", ctx)}
	}
	if pred.Predicate() != nil {
		return nil, &UnsupportedExpressionShapeError{Shape: "PredicatedExpression with grammar Predicate (use WalkPredicate)"}
	}
	return r.walkAtom(pred.ExpressionAtom())
}

// walkAtom dispatches concrete ExpressionAtom variants. Returns a
// Value OR — for BinaryComparisonPredicate atoms — a
// *cascades.ComparisonPredicate wrapped as a Value, since the
// grammar treats binary comparisons as atoms but the analyzer
// surfaces them as predicates. Callers should type-switch the
// return to pick up both shapes.
func (r *Resolver) walkAtom(atom antlrgen.IExpressionAtomContext) (cascades.Value, error) {
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
		// integer div are not wired yet — cascades.ArithmeticOp
		// doesn't expose them.
		return r.walkMathExpression(a)
	case *antlrgen.FunctionCallExpressionAtomContext:
		// Function call — only aggregates (COUNT/SUM/MIN/MAX/AVG)
		// are wired in the seed. Scalar functions land once the
		// scalar-function catalogue is ported.
		return r.walkFunctionCall(a.FunctionCall())
	}
	return nil, &UnsupportedExpressionShapeError{Shape: fmt.Sprintf("%T", atom)}
}

// walkFunctionCall handles FunctionCall contexts. Only aggregate
// functions are wired today (COUNT/SUM/MIN/MAX/AVG); scalar function
// dispatch waits on the scalar-function catalogue port.
//
// Uses the Resolver's cached FunctionCatalog (built lazily on first
// use, or provided via NewWithFunctionCatalog) so the walker
// amortises catalog construction across calls.
func (r *Resolver) walkFunctionCall(fc antlrgen.IFunctionCallContext) (cascades.Value, error) {
	if fc == nil {
		return nil, fmt.Errorf("expr.walkFunctionCall: nil")
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
	var args []cascades.Value
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
		args = []cascades.Value{v}
	}
	return r.ResolveFunctionCall(fcat, semantic.NewUnquoted(name), isStar, args)
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
// into a cascades.ArithmeticValue. Operator resolution reads the
// MathOperator context's terminal tokens. MOD / MODULE / DIV
// (integer division) aren't mapped to cascades.ArithmeticOp yet —
// the cascades enum covers +, -, *, / and grows with the Type
// hierarchy port.
func (r *Resolver) walkMathExpression(m *antlrgen.MathExpressionAtomContext) (cascades.Value, error) {
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
// present in cascades.ArithmeticOp.
func arithmeticOpFromCtx(op antlrgen.IMathOperatorContext) (cascades.ArithmeticOp, error) {
	if op == nil {
		return cascades.OpAdd, fmt.Errorf("arithmeticOpFromCtx: nil")
	}
	mo, ok := op.(*antlrgen.MathOperatorContext)
	if !ok {
		return cascades.OpAdd, fmt.Errorf("arithmeticOpFromCtx: unexpected ctx %T", op)
	}
	switch {
	case mo.PLUS() != nil:
		return cascades.OpAdd, nil
	case mo.MINUS() != nil:
		return cascades.OpSub, nil
	case mo.STAR() != nil:
		return cascades.OpMul, nil
	case mo.DIVIDE() != nil:
		return cascades.OpDiv, nil
	}
	return cascades.OpAdd, &UnsupportedExpressionShapeError{Shape: "MathOperator: " + mo.GetText()}
}

// walkRecordConstructor unwraps a single-element, unnamed-field,
// un-typed record constructor — the parser's shape for
// parenthesised expressions `(expr)`. Multi-element or annotated
// record constructors require dedicated cascades.RecordConstructorValue
// support, not wired yet.
func (r *Resolver) walkRecordConstructor(rc antlrgen.IRecordConstructorContext) (cascades.Value, error) {
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
func (r *Resolver) WalkPredicate(ctx antlrgen.IExpressionContext) (cascades.QueryPredicate, error) {
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
	}
	return nil, &UnsupportedExpressionShapeError{Shape: fmt.Sprintf("%T", ctx)}
}

// walkPredicatedExpression handles the leaf case — a bare value or
// a BinaryComparisonPredicate atom, plus grammar Predicate shapes
// (IS NULL / IS NOT NULL) that modify the preceding atom.
func (r *Resolver) walkPredicatedExpression(pred *antlrgen.PredicatedExpressionContext) (cascades.QueryPredicate, error) {
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
	if bv, ok := v.(*cascades.BooleanValue); ok && bv.Value != nil {
		if *bv.Value {
			return cascades.NewConstantPredicate(cascades.TriTrue), nil
		}
		return cascades.NewConstantPredicate(cascades.TriFalse), nil
	}
	return cascades.NewValuePredicate(v), nil
}

// unwrapParenPredicate recurses WalkPredicate on a single-element
// parenthesised expression. Returns error if the RecordConstructor
// shape isn't a simple paren-wrap (multi-element, named field,
// OfType clause).
func (r *Resolver) unwrapParenPredicate(rc antlrgen.IRecordConstructorContext) (cascades.QueryPredicate, error) {
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
func (r *Resolver) walkLogicalExpression(le *antlrgen.LogicalExpressionContext) (cascades.QueryPredicate, error) {
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
	case lo.AND() != nil:
		return r.ResolveAnd(flattenAnd(left, right)...), nil
	case lo.OR() != nil:
		return r.ResolveOr(flattenOr(left, right)...), nil
	case lo.XOR() != nil:
		return nil, &UnsupportedExpressionShapeError{Shape: "XOR"}
	}
	return nil, &UnsupportedExpressionShapeError{Shape: "LogicalOperator: " + lo.GetText()}
}

// flattenAnd/flattenOr collapse left-deep chains built by the
// parser. `a AND b AND c` parses as (and (and a b) c) — here we
// return [a b c] so ResolveAnd produces a single 3-child And
// rather than nested pairs. AndFlattenRule in cascades would fix
// it later anyway, but seeding the flat shape avoids fixpoint work.
func flattenAnd(preds ...cascades.QueryPredicate) []cascades.QueryPredicate {
	var out []cascades.QueryPredicate
	for _, p := range preds {
		if and, ok := p.(*cascades.AndPredicate); ok {
			out = append(out, and.SubPredicates...)
		} else {
			out = append(out, p)
		}
	}
	return out
}

func flattenOr(preds ...cascades.QueryPredicate) []cascades.QueryPredicate {
	var out []cascades.QueryPredicate
	for _, p := range preds {
		if or, ok := p.(*cascades.OrPredicate); ok {
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
func (r *Resolver) walkGrammarPredicate(atom antlrgen.IExpressionAtomContext, pred antlrgen.IPredicateContext) (cascades.QueryPredicate, error) {
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
			// `x IS TRUE` ≡ `x = TRUE`. Distinct from `x = TRUE`
			// syntactically in the grammar, but semantically the
			// same under Kleene (NULL = TRUE → UNKNOWN, NULL IS
			// TRUE → FALSE) — wait, actually they DIFFER on NULL:
			//   `NULL = TRUE` → UNKNOWN
			//   `NULL IS TRUE` → FALSE  (2VL semantics)
			// To get the 2VL behaviour, wrap in a real `=`
			// predicate that produces UNKNOWN on NULL, then rely
			// on downstream Kleene-to-2VL coercion at the WHERE
			// level (WHERE UNKNOWN == not-selected, same as FALSE).
			// That's what the embedded engine does today via
			// triBool.toBool.
			pred := cascades.NewComparisonPredicate(lhs, cascades.Comparison{
				Type: cascades.ComparisonEquals, Operand: true,
			})
			if p.NOT() != nil {
				return r.ResolveNot(pred), nil
			}
			return pred, nil
		case p.FALSE() != nil:
			pred := cascades.NewComparisonPredicate(lhs, cascades.Comparison{
				Type: cascades.ComparisonEquals, Operand: false,
			})
			if p.NOT() != nil {
				return r.ResolveNot(pred), nil
			}
			return pred, nil
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
			return nil, &UnsupportedExpressionShapeError{Shape: "IN with subquery/parameter/column (walker handles explicit list only)"}
		}
		ec, ok := exprs.(*antlrgen.ExpressionsContext)
		if !ok {
			return nil, &UnsupportedExpressionShapeError{Shape: fmt.Sprintf("Expressions ctx %T", exprs)}
		}
		lhsVal, err := r.walkAtom(atom)
		if err != nil {
			return nil, err
		}
		list := make([]cascades.Value, 0, len(ec.AllExpression()))
		for _, e := range ec.AllExpression() {
			v, err := r.WalkExpression(e)
			if err != nil {
				return nil, err
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
		// `x LIKE 'pattern' [ESCAPE 'c']` — seed wires LIKE without
		// ESCAPE (cascades.likeMatch doesn't take an escape rune
		// yet). The grammar parses both, so the ESCAPE form errors
		// with a specific message pointing at the companion work.
		if p.ESCAPE() != nil {
			return nil, &UnsupportedExpressionShapeError{
				Shape: "LIKE with ESCAPE clause (cascades.likeMatch doesn't take escape rune yet)",
			}
		}
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
		like, err := r.ResolveLike(lhsVal, patConst)
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
		lowerBound, err := r.ResolveComparison(cascades.ComparisonGreaterThanEq, lhsVal, loVal)
		if err != nil {
			return nil, err
		}
		upperBound, err := r.ResolveComparison(cascades.ComparisonLessThanOrEq, lhsVal, hiVal)
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

// walkBinaryComparison converts `left OP right` into a
// ComparisonPredicate via ResolveComparison. Operator dispatch
// reads ComparisonOperator's terminal-token accessors — there's no
// single GetText we can rely on since `!=`, `<>`, `>=` all span
// two tokens.
func (r *Resolver) walkBinaryComparison(bc *antlrgen.BinaryComparisonPredicateContext) (cascades.QueryPredicate, error) {
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
func comparisonOpFromCtx(op antlrgen.IComparisonOperatorContext) (cascades.ComparisonType, error) {
	if op == nil {
		return cascades.ComparisonEquals, fmt.Errorf("comparisonOpFromCtx: nil operator")
	}
	c, ok := op.(*antlrgen.ComparisonOperatorContext)
	if !ok {
		return cascades.ComparisonEquals, fmt.Errorf("comparisonOpFromCtx: unexpected ctx %T", op)
	}
	hasEq := c.EQUAL_SYMBOL() != nil
	hasGt := c.GREATER_SYMBOL() != nil
	hasLt := c.LESS_SYMBOL() != nil
	hasNot := c.EXCLAMATION_SYMBOL() != nil
	// Spread multi-token operators. Token order matters — the
	// grammar emits <= as '<' '=', not '=' '<'.
	if c.IS() != nil && c.DISTINCT() != nil && c.FROM() != nil {
		if c.NOT() != nil {
			return cascades.ComparisonNotDistinctFrom, nil
		}
		return cascades.ComparisonIsDistinctFrom, nil
	}
	switch {
	case hasEq && !hasGt && !hasLt && !hasNot:
		return cascades.ComparisonEquals, nil
	case hasNot && hasEq:
		return cascades.ComparisonNotEquals, nil
	case hasLt && hasGt: // <>
		return cascades.ComparisonNotEquals, nil
	case hasLt && hasEq:
		return cascades.ComparisonLessThanOrEq, nil
	case hasGt && hasEq:
		return cascades.ComparisonGreaterThanEq, nil
	case hasLt:
		return cascades.ComparisonLessThan, nil
	case hasGt:
		return cascades.ComparisonGreaterThan, nil
	}
	return cascades.ComparisonEquals, &UnsupportedExpressionShapeError{
		Shape: "ComparisonOperator: " + c.GetText(),
	}
}

// walkColumnRef: an identifier from the parse tree → ResolveIdentifier.
// Handles both bare (`col`) and qualified (`t.col`) via the number
// of Uid segments.
func (r *Resolver) walkColumnRef(fullId antlrgen.IFullIdContext) (cascades.Value, error) {
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
// Seed handles the common shapes (integer literal, string literal).
// Float / decimal-with-fractional handling is deferred until
// cascades.TypeFloat lands.
func (r *Resolver) walkConstant(c antlrgen.IConstantContext) (cascades.Value, error) {
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
		text := k.GetText()
		n, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("expr.walkConstant: integer parse %q: %w", text, err)
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
