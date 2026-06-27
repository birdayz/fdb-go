package embedded

import (
	antlrgen "fdb.dev/pkg/relational/core/parser/gen"
)

// Operator classifiers shared by the expression evaluators
// (eval_map.go, eval_proto.go, eval_predicate*.go). Each maps a typed
// ANTLR operator node to a canonical string without GetText().

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

// classifyMathOp returns a canonical string for arithmetic operators
// using typed ANTLR terminal nodes (no GetText()).
func classifyMathOp(op antlrgen.IMathOperatorContext) string {
	if op == nil {
		return ""
	}
	m, ok := op.(*antlrgen.MathOperatorContext)
	if !ok {
		return ""
	}
	switch {
	case m.PLUS() != nil:
		return "+"
	case m.MINUS() != nil:
		return "-"
	case m.STAR() != nil:
		return "*"
	case m.DIVIDE() != nil:
		return "/"
	case m.DIV() != nil:
		return "DIV"
	case m.MODULE() != nil:
		return "%"
	case m.MOD() != nil:
		return "MOD"
	}
	return ""
}

// classifyBitOp returns a canonical string for bitwise operators
// using typed ANTLR terminal nodes (no GetText()).
func classifyBitOp(op antlrgen.IBitOperatorContext) string {
	if op == nil {
		return ""
	}
	b, ok := op.(*antlrgen.BitOperatorContext)
	if !ok {
		return ""
	}
	switch {
	case b.BIT_AND_OP() != nil:
		return "&"
	case b.BIT_OR_OP() != nil:
		return "|"
	case b.BIT_XOR_OP() != nil:
		return "^"
	case len(b.AllLESS_SYMBOL()) >= 2:
		return "<<"
	case len(b.AllGREATER_SYMBOL()) >= 2:
		return ">>"
	}
	return ""
}
