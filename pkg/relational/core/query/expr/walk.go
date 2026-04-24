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
// Dispatches by concrete ANTLR context type to the right Resolver
// method.
//
// Seed scope — ONLY these shapes are handled:
//
//   - PredicatedExpression wrapping an ExpressionAtom (no predicate).
//   - ExpressionAtom = FullColumnName → column reference.
//   - ExpressionAtom = Constant (integer / string literal only).
//
// Everything else returns UnsupportedExpressionShapeError so the
// caller can fall back to the existing logical-builder path. The
// full walker (arithmetic, logical AND/OR, comparisons, function
// calls, nested subqueries) lands in follow-up commits; this seed
// establishes the dispatch shape.
func (r *Resolver) WalkExpression(ctx antlrgen.IExpressionContext) (cascades.Value, error) {
	if ctx == nil {
		return nil, fmt.Errorf("expr.WalkExpression: nil context")
	}
	pred, ok := ctx.(*antlrgen.PredicatedExpressionContext)
	if !ok {
		return nil, &UnsupportedExpressionShapeError{Shape: fmt.Sprintf("%T", ctx)}
	}
	if pred.Predicate() != nil {
		// BETWEEN / IN / LIKE / IS NULL / ... predicates — not wired yet.
		return nil, &UnsupportedExpressionShapeError{Shape: "PredicatedExpression with Predicate()"}
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
	}
	return nil, &UnsupportedExpressionShapeError{Shape: fmt.Sprintf("%T", atom)}
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
		if p.NULL_LITERAL() == nil {
			// `x IS TRUE` / `x IS FALSE` are legal but distinct
			// semantics — not wired yet.
			return nil, &UnsupportedExpressionShapeError{Shape: "IS expression without NULL literal"}
		}
		lhs, err := r.walkAtom(atom)
		if err != nil {
			return nil, err
		}
		if p.NOT() != nil {
			return r.ResolveIsNotNull(lhs)
		}
		return r.ResolveIsNull(lhs)
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
