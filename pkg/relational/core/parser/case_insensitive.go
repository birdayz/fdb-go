package parser

import (
	"unicode"

	"github.com/antlr4-go/antlr/v4"
)

// caseInsensitiveCharStream wraps an antlr.CharStream and upper-cases every
// code point returned by LA(). The underlying stream keeps the original text
// so GetText(...) calls still return identifier text in its source casing.
//
// Mirrors Java's com.apple.foundationdb.relational.recordlayer.query.
// CaseInsensitiveCharStream. Needed because the Relational lexer grammar is
// case-sensitive and expects an upstream layer to normalise keyword casing.
type caseInsensitiveCharStream struct {
	antlr.CharStream
}

// newCaseInsensitiveCharStream builds a case-folding CharStream over sql.
func newCaseInsensitiveCharStream(sql string) *caseInsensitiveCharStream {
	return &caseInsensitiveCharStream{CharStream: antlr.NewInputStream(sql)}
}

// LA returns the i-th look-ahead code point, upper-cased. Sentinel values
// (EOF == -1 and 0) pass through unchanged so lexer state machines keep
// recognising them.
func (c *caseInsensitiveCharStream) LA(i int) int {
	r := c.CharStream.LA(i)
	if r <= 0 {
		return r
	}
	return int(unicode.ToUpper(rune(r)))
}
