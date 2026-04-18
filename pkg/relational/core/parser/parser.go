// Package parser — public wrapper around the ANTLR-generated Relational SQL
// parser and lexer. Consumers call Parse(sql) and receive a parse tree ready
// for the semantic analyzer.
package parser

import (
	"strings"

	"github.com/antlr4-go/antlr/v4"

	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser/gen"
)

// Parse runs the Relational SQL lexer + parser over sql and returns the
// resulting parse tree root. Keywords are matched case-insensitively; the
// original source casing is preserved in token text (identifiers, literals).
//
// On failure Parse returns an *api.Error with Code == api.ErrCodeSyntaxError.
// The Message is of the form
//
//	syntax error:
//	<source line containing the offending token>
//	<spaces><^^^ underlining the token>
//
// Matches Java's com.apple.foundationdb.relational.recordlayer.query.
// QueryParser.parse(): Java reports only the FIRST syntax error and runs
// ParseHelpers.underlineParsingError to produce the underline. Downstream
// errors after a syntax failure are usually cascade noise.
func Parse(sql string) (antlrgen.IRootContext, error) {
	input := newCaseInsensitiveCharStream(sql)

	lexer := antlrgen.NewRelationalLexer(input)
	listener := &collectingErrorListener{sql: sql}
	lexer.RemoveErrorListeners()
	lexer.AddErrorListener(listener)

	tokens := antlr.NewCommonTokenStream(lexer, antlr.TokenDefaultChannel)

	p := antlrgen.NewRelationalParser(tokens)
	p.RemoveErrorListeners()
	p.AddErrorListener(listener)

	root := p.Root()

	if len(listener.errs) > 0 {
		return nil, buildSyntaxError(listener.errs[0])
	}
	return root, nil
}

// syntaxError holds everything we need to reproduce Java's
// ParseHelpers.underlineParsingError output for a single lexer/parser
// failure.
type syntaxError struct {
	line      int    // 1-based
	column    int    // 0-based
	msg       string // ANTLR's "mismatched input ..." / "token recognition ..." text
	sourceSQL string // full source so we can slice the offending line
	token     antlr.Token
}

type collectingErrorListener struct {
	*antlr.DefaultErrorListener
	sql  string
	errs []syntaxError
}

// SyntaxError records a single syntax error. ANTLR calls this from the
// lexer / parser on every recognition failure, with a 1-based line and
// 0-based column.
func (l *collectingErrorListener) SyntaxError(
	_ antlr.Recognizer,
	offendingSymbol any,
	line, column int,
	msg string,
	_ antlr.RecognitionException,
) {
	tok, _ := offendingSymbol.(antlr.Token)
	l.errs = append(l.errs, syntaxError{
		line:      line,
		column:    column,
		msg:       msg,
		sourceSQL: l.sql,
		token:     tok,
	})
}

func buildSyntaxError(e syntaxError) *api.Error {
	return api.NewError(api.ErrCodeSyntaxError, "syntax error:\n"+underlineParsingError(e))
}

// underlineParsingError mirrors Java's ParseHelpers.underlineParsingError:
// emit the source line containing the offending token, followed by a
// blank-padded line with "^" under each character of the token (or "^^"
// if stop < start, which ANTLR uses for a missing token).
//
// Recipe comes from "The Definitive ANTLR 4 Reference, 2nd Edition" —
// Java calls it out explicitly in ParseHelpers.java.
func underlineParsingError(e syntaxError) string {
	var b strings.Builder

	lines := strings.Split(e.sourceSQL, "\n")
	if e.line >= 1 && e.line <= len(lines) {
		b.WriteString(lines[e.line-1])
	}
	b.WriteByte('\n')

	for i := 0; i < e.column; i++ {
		b.WriteByte(' ')
	}

	if e.token == nil {
		// No token info (lexer-level recognition error). Java's path
		// still tries offendingSymbol.getStartIndex/StopIndex; without
		// it we emit a single caret to mark the column.
		b.WriteByte('^')
		return b.String()
	}
	start := e.token.GetStart()
	stop := e.token.GetStop()
	switch {
	case stop < start:
		// Java: "^^" marks a missing token.
		b.WriteString("^^")
	case start >= 0:
		for i := 0; i < stop-start+1; i++ {
			b.WriteByte('^')
		}
	}
	return b.String()
}
