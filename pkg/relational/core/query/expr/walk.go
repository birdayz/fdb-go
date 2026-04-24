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

// walkAtom dispatches concrete ExpressionAtom variants.
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
