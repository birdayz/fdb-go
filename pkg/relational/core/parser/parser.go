// Package parser — public wrapper around the ANTLR-generated Relational SQL
// parser and lexer. Consumers call Parse(sql) and receive a parse tree ready
// for the semantic analyzer.
package parser

import (
	"fmt"
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
// The Message lists every syntax error reported by ANTLR in "line:col: msg"
// form, one per line, in the order they were produced.
func Parse(sql string) (antlrgen.IRootContext, error) {
	input := newCaseInsensitiveCharStream(sql)

	lexer := antlrgen.NewRelationalLexer(input)
	lexErrs := &collectingErrorListener{}
	lexer.RemoveErrorListeners()
	lexer.AddErrorListener(lexErrs)

	tokens := antlr.NewCommonTokenStream(lexer, antlr.TokenDefaultChannel)

	p := antlrgen.NewRelationalParser(tokens)
	parseErrs := &collectingErrorListener{}
	p.RemoveErrorListeners()
	p.AddErrorListener(parseErrs)

	root := p.Root()

	// Report lexer errors first — if the token stream is malformed, parser
	// errors downstream are usually noise. Callers can still inspect both
	// sets via the returned api.Error.Context map.
	combined := append([]syntaxError(nil), lexErrs.errs...)
	combined = append(combined, parseErrs.errs...)
	if len(combined) > 0 {
		return nil, buildSyntaxError(combined)
	}
	return root, nil
}

type syntaxError struct {
	line   int
	column int
	msg    string
}

func (s syntaxError) String() string {
	return fmt.Sprintf("%d:%d: %s", s.line, s.column, s.msg)
}

type collectingErrorListener struct {
	*antlr.DefaultErrorListener
	errs []syntaxError
}

// SyntaxError records a single syntax error. ANTLR calls this from the
// lexer / parser on every recognition failure, with a 1-based line and
// 0-based column.
func (l *collectingErrorListener) SyntaxError(
	_ antlr.Recognizer,
	_ any,
	line, column int,
	msg string,
	_ antlr.RecognitionException,
) {
	l.errs = append(l.errs, syntaxError{line: line, column: column, msg: msg})
}

func buildSyntaxError(errs []syntaxError) *api.Error {
	var b strings.Builder
	for i, e := range errs {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(e.String())
	}
	return api.NewError(api.ErrCodeSyntaxError, b.String())
}
