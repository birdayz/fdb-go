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
	}
	return nil, &UnsupportedExpressionShapeError{Shape: fmt.Sprintf("%T", atom)}
}

// WalkPredicate is the dual of WalkExpression — returns a cascades
// QueryPredicate for an expression that's semantically boolean. For
// bare columns (`WHERE flag`) it wraps the resolved value in a
// ValuePredicate; for binary comparisons (`WHERE id = 1`) it
// produces a ComparisonPredicate via ResolveComparison.
//
// Seed scope matches WalkExpression's: PredicatedExpression only.
// Binary comparisons dispatch via walkBinaryComparison; all other
// predicate shapes (BETWEEN, IN, LIKE, IS NULL via grammar's
// Predicate node) return UnsupportedExpressionShapeError.
func (r *Resolver) WalkPredicate(ctx antlrgen.IExpressionContext) (cascades.QueryPredicate, error) {
	if ctx == nil {
		return nil, fmt.Errorf("expr.WalkPredicate: nil context")
	}
	pred, ok := ctx.(*antlrgen.PredicatedExpressionContext)
	if !ok {
		return nil, &UnsupportedExpressionShapeError{Shape: fmt.Sprintf("%T", ctx)}
	}
	if pred.Predicate() != nil {
		// Grammar-level Predicate nodes (BETWEEN, IN, LIKE, IS NULL)
		// not wired yet.
		return nil, &UnsupportedExpressionShapeError{Shape: "PredicatedExpression with grammar Predicate"}
	}
	// The expression-atom may itself be a BinaryComparisonPredicate;
	// dispatch here.
	atom := pred.ExpressionAtom()
	if bc, ok := atom.(*antlrgen.BinaryComparisonPredicateContext); ok {
		return r.walkBinaryComparison(bc)
	}
	// Bare value atom → ValuePredicate.
	v, err := r.walkAtom(atom)
	if err != nil {
		return nil, err
	}
	return cascades.NewValuePredicate(v), nil
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
