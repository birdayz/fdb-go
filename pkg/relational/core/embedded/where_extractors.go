package embedded

import (
	"context"
	"strings"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/functions"
	antlrgen "github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser/gen"
)

// classifyComparisonOp returns a canonical string for comparison operators
// using typed ANTLR terminal nodes (no GetText()). Returns "" for
// unrecognized operators.
func classifyComparisonOp(op antlrgen.IComparisonOperatorContext) string {
	if op == nil {
		return ""
	}
	c, ok := op.(*antlrgen.ComparisonOperatorContext)
	if !ok {
		return ""
	}
	if c.IS() != nil && c.DISTINCT() != nil {
		if c.NOT() != nil {
			return "IS NOT DISTINCT FROM"
		}
		return "IS DISTINCT FROM"
	}
	hasEq := c.EQUAL_SYMBOL() != nil
	hasGt := c.GREATER_SYMBOL() != nil
	hasLt := c.LESS_SYMBOL() != nil
	hasBang := c.EXCLAMATION_SYMBOL() != nil
	switch {
	case hasBang && hasEq:
		return "!="
	case hasLt && hasGt:
		return "<>"
	case hasGt && hasEq:
		return ">="
	case hasLt && hasEq:
		return "<="
	case hasEq && !hasGt && !hasLt:
		return "="
	case hasGt && !hasEq:
		return ">"
	case hasLt && !hasEq && !hasGt:
		return "<"
	default:
		return ""
	}
}

// Pushdown-path predicate extractors.
//
// Thin parse-tree-aware helpers that every pushdown shape in
// pk_pushdown.go / secondary_index_pushdown.go / in_list_pushdown.go
// / like_prefix_pushdown.go / pk_prefix_pushdown.go calls to peek
// at an AND leaf and decide whether it's a shape we can narrow on.
//
// extractColOpLiteral       `col {=,>,>=,<,<=} literal` (col-on-left
//                            normalised via flipComparisonOp)
// extractColBetweenLiteral  `col BETWEEN lo AND hi`
// extractColEqualsLiteral   `col = literal` (tighter variant kept
//                            for the bare-equality path)
// extractColumnRef          bare-identifier unwrap (rejects
//                            function calls, subqueries, etc.)
// evalConstantAtom          evaluate a constant-expression RHS
//                            without a row context; returns (nil, true)
//                            for legitimate NULL and (_, false) on
//                            non-constants so callers can bail
// flattenAndPredicates      AND chain → []leaf (OR bails the caller)
// flipComparisonOp          `<` ↔ `>` etc., used when the literal
//                            appears on the LHS
// extractPKUserFields       walks a KeyExpression and returns the
//                            ordered PK user-field names (stripping
//                            the record-type-key prefix)
//
// All extractors are pure and reusable — no state on
// EmbeddedConnection beyond the evalConstantAtom call site which
// uses c for error-surface hooks.

// extractColOpLiteral generalises extractColEqualsLiteral to any
// comparison operator among `=`, `>`, `>=`, `<`, `<=`. Returns the
// operator text (one of the above), the bare column name, and the
// literal value. When the RHS side is the column and the LHS is the
// literal, the operator is flipped to preserve col-on-left semantics
// (so `5 < id` becomes `id > 5`).
func extractColOpLiteral(
	ctx context.Context,
	c *EmbeddedConnection,
	expr antlrgen.IExpressionContext,
) (op, col string, val any, ok bool) {
	pred, good := expr.(*antlrgen.PredicatedExpressionContext)
	if !good {
		return "", "", nil, false
	}
	if pred.Predicate() != nil {
		return "", "", nil, false
	}
	bcp, good := pred.ExpressionAtom().(*antlrgen.BinaryComparisonPredicateContext)
	if !good {
		return "", "", nil, false
	}
	opC := bcp.ComparisonOperator()
	if opC == nil {
		return "", "", nil, false
	}
	opText := classifyComparisonOp(opC)
	switch opText {
	case "=", ">", ">=", "<", "<=":
	default:
		return "", "", nil, false
	}
	// Column-on-left, literal-on-right.
	if name, isCol := extractColumnRef(bcp.GetLeft()); isCol {
		if v, isLit := evalConstantAtom(ctx, c, bcp.GetRight()); isLit {
			return opText, name, v, true
		}
	}
	// Column-on-right, literal-on-left — flip the operator.
	if name, isCol := extractColumnRef(bcp.GetRight()); isCol {
		if v, isLit := evalConstantAtom(ctx, c, bcp.GetLeft()); isLit {
			return flipComparisonOp(opText), name, v, true
		}
	}
	return "", "", nil, false
}

// extractColBetweenLiteral recognises `col BETWEEN lo AND hi` where
// col is a bare column reference and lo, hi are constant literals.
// Returns the column name and both literal values. NOT BETWEEN,
// non-constant bounds, and NULL bounds bail out — NOT BETWEEN is a
// non-contiguous key range (two open half-ranges), and a NULL bound
// makes the predicate UNKNOWN per SQL 3VL so it cannot narrow the
// scan. BETWEEN's SQL semantics are inclusive on both sides, which
// the callers translate into `col >= lo AND col <= hi` bounds.
func extractColBetweenLiteral(
	ctx context.Context,
	c *EmbeddedConnection,
	expr antlrgen.IExpressionContext,
) (col string, lo, hi any, ok bool) {
	pred, good := expr.(*antlrgen.PredicatedExpressionContext)
	if !good {
		return "", nil, nil, false
	}
	bet, good := pred.Predicate().(*antlrgen.BetweenComparisonPredicateContext)
	if !good {
		return "", nil, nil, false
	}
	if bet.NOT() != nil {
		return "", nil, nil, false
	}
	name, isCol := extractColumnRef(pred.ExpressionAtom())
	if !isCol {
		return "", nil, nil, false
	}
	loVal, isLit := evalConstantAtom(ctx, c, bet.GetLeft())
	if !isLit || loVal == nil {
		return "", nil, nil, false
	}
	hiVal, isLit := evalConstantAtom(ctx, c, bet.GetRight())
	if !isLit || hiVal == nil {
		return "", nil, nil, false
	}
	return name, loVal, hiVal, true
}

// flipComparisonOp flips a comparison operator for the case where
// the column ref appears on the right (`5 < id` → treat as `id > 5`).
func flipComparisonOp(op string) string {
	switch op {
	case ">":
		return "<"
	case ">=":
		return "<="
	case "<":
		return ">"
	case "<=":
		return ">="
	}
	return op // `=` is symmetric
}

// extractPKUserFields returns the ordered list of user field names
// making up the primary key when pushdown is safe, or nil otherwise.
//
// Only CompositeKeyExpression is supported today: SQL DDL's default
// (non-intermingled) path emits `Concat(RecordTypeKey, Field(col)…)`,
// and the RecordTypeKey prefix in the range tuple naturally scopes
// the FDB scan to the right record type. The bare FieldKeyExpression
// branch — which SQL DDL only emits for `SetIntermingleTables(true)`
// schemas — has NO type filter; an intermingled multi-table schema
// where different types share a PK column space could return a
// wrong-typed record at the same key. We bail on that shape until
// a type-filtering wrapper is added; the scan path still handles
// intermingled tables correctly.
func extractPKUserFields(pk recordlayer.KeyExpression) []string {
	if e, ok := pk.(*recordlayer.CompositeKeyExpression); ok {
		// FieldNames() on a CompositeKeyExpression returns just the
		// Field children, not the RecordTypeKey (which contributes no
		// named column). That's exactly the user field list.
		return e.FieldNames()
	}
	return nil
}

// flattenAndPredicates walks a WHERE expression as a conjunction of
// leaf predicates. Returns the flat list of leaves and `true` on
// success. Fails (returns false) when a non-AND logical operator
// (OR, XOR, NOT) appears AS THE TOP-LEVEL connective at any position
// the recursion is flattening — those break the "everything the scan
// would also have matched" invariant that pushdown relies on.
//
// LAYERED CONTRACT: nested ORs inside a parenthesised composite
// (`a = 1 AND (b = 2 OR c = 3)`) are OPAQUE to this walker — the
// parenthesised expression isn't a LogicalExpressionContext, so the
// recursion treats it as a leaf and returns true. The next layer
// (extractColOpLiteral / extractColEqualsLiteral / etc.) is the one
// that rejects shapes it can't push down. Each layer owns a
// different boundary: this function owns the connective shape; the
// extractors own the leaf shape.
//
// Pinned by where_extractors_test.go's TestFlattenAndPredicates_*
// tests (top-level OR fails, nested OR-in-parens succeeds as opaque
// leaf).
func flattenAndPredicates(expr antlrgen.IExpressionContext) ([]antlrgen.IExpressionContext, bool) {
	le, ok := expr.(*antlrgen.LogicalExpressionContext)
	if !ok {
		return []antlrgen.IExpressionContext{expr}, true
	}
	op := le.LogicalOperator()
	isAnd := op.AND() != nil || len(op.AllBIT_AND_OP()) >= 2
	if !isAnd {
		return nil, false
	}
	left, lok := flattenAndPredicates(le.Expression(0))
	if !lok {
		return nil, false
	}
	right, rok := flattenAndPredicates(le.Expression(1))
	if !rok {
		return nil, false
	}
	return append(left, right...), true
}

// extractColEqualsLiteral returns (colName, literalValue, true) when
// the expression is exactly a `col = literal` equality. NULL on the
// RHS and any non-constant RHS cause a `false` return, in which case
// the pushdown caller falls back to the full scan.
func extractColEqualsLiteral(
	ctx context.Context,
	c *EmbeddedConnection,
	expr antlrgen.IExpressionContext,
) (string, any, bool) {
	pred, ok := expr.(*antlrgen.PredicatedExpressionContext)
	if !ok {
		return "", nil, false
	}
	if pred.Predicate() != nil {
		return "", nil, false
	}
	bcp, ok := pred.ExpressionAtom().(*antlrgen.BinaryComparisonPredicateContext)
	if !ok {
		return "", nil, false
	}
	if classifyComparisonOp(bcp.ComparisonOperator()) != "=" {
		return "", nil, false
	}
	// One side must be a column ref; the other must evaluate to a
	// literal. Try both orderings.
	if colName, ok := extractColumnRef(bcp.GetLeft()); ok {
		if val, ok := evalConstantAtom(ctx, c, bcp.GetRight()); ok {
			return colName, val, true
		}
	}
	if colName, ok := extractColumnRef(bcp.GetRight()); ok {
		if val, ok := evalConstantAtom(ctx, c, bcp.GetLeft()); ok {
			return colName, val, true
		}
	}
	return "", nil, false
}

// extractColumnRef returns the bare (last-segment) column name from a
// FullColumnName expression atom.
func extractColumnRef(atom antlrgen.IExpressionAtomContext) (string, bool) {
	fcn, ok := atom.(*antlrgen.FullColumnNameExpressionAtomContext)
	if !ok {
		return "", false
	}
	name := functions.FullIdToName(fcn.FullColumnName().FullId())
	return name[strings.LastIndex(name, ".")+1:], true
}

// evalConstantAtom attempts to evaluate an expression atom without a
// row context. Succeeds for literals / bound params / pure-constant
// expressions; fails otherwise (including for NULL, since NULL on the
// RHS of `=` is never true under three-valued logic and should fall
// back to scan for consistent semantics).
func evalConstantAtom(ctx context.Context, c *EmbeddedConnection, atom antlrgen.IExpressionAtomContext) (any, bool) {
	v, err := evalExprAtom(ctx, c, nil, atom)
	if err != nil {
		return nil, false
	}
	if v == nil {
		return nil, false
	}
	return v, true
}
