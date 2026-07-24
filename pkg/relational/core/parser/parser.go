// Package parser — public wrapper around the ANTLR-generated Relational SQL
// parser and lexer. Consumers call Parse(sql) and receive a parse tree ready
// for the semantic analyzer. Returned contexts are read-only parse trees; the
// completed parser reachable through a generated context's GetParser method is
// retained for metadata and tree rendering, not for invoking another rule.
package parser

import (
	"fmt"
	"runtime"
	"strings"
	"sync"

	"github.com/antlr4-go/antlr/v4"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/parser/gen"
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
func Parse(sql string) (ctx antlrgen.IRootContext, err error) {
	if maxNesting(sql) > 500 {
		return nil, &api.Error{
			Code:    api.ErrCodeSyntaxError,
			Message: "syntax error: expression nesting exceeds maximum depth (500)",
		}
	}
	defer func() {
		if r := recover(); r != nil {
			ctx = nil
			err = &api.Error{
				Code:    api.ErrCodeSyntaxError,
				Message: fmt.Sprintf("parse panic: %v", r),
			}
		}
	}()
	p, listener, releasePredictionState := newParser(sql, nil)
	defer releasePredictionState()
	root := p.Root()
	if len(listener.errs) > 0 {
		return nil, buildSyntaxError(listener.errs[0])
	}
	return root, nil
}

// maxNesting returns the maximum parenthesis nesting depth in sql.
func maxNesting(sql string) int {
	max, cur := 0, 0
	for _, c := range sql {
		switch c {
		case '(':
			cur++
			if cur > max {
				max = cur
			}
		case ')':
			if cur > 0 {
				cur--
			}
		}
	}
	return max
}

// ParseFunction parses a CREATE FUNCTION ... statement and returns
// the SqlInvokedFunction parse tree (the body after the CREATE
// keyword). Mirrors Java's QueryParser.parseFunction. The "CREATE"
// token is consumed before the sqlInvokedFunction rule runs because
// the grammar's sqlInvokedFunction starts after CREATE — so the
// token stream needs to be advanced once.
//
// On empty or malformed input the pre-consumption of CREATE would
// otherwise panic ("cannot consume EOF" from ANTLR); guard by peeking
// the stream and reporting a clean syntax error instead.
func ParseFunction(sql string) (ctx antlrgen.ISqlInvokedFunctionContext, err error) {
	// ANTLR's generated rules and tokenization can panic on genuinely
	// adversarial input. Surface any such panic as a syntax error rather than
	// propagating it to the caller.
	defer func() {
		if r := recover(); r != nil {
			ctx = nil
			err = &api.Error{
				Code:    api.ErrCodeSyntaxError,
				Message: fmt.Sprintf("parse function panic: %v", r),
			}
		}
	}()
	p, listener, releasePredictionState := newParser(sql, func(ts *antlr.CommonTokenStream) {
		// Pre-populate the stream so Consume has something to consume.
		ts.Fill()
		// If the stream is empty (e.g., sql=""), Consume() panics in
		// ANTLR. Skip the pre-consume — the parser will still emit a
		// syntax error for the missing CREATE keyword.
		if ts.LA(1) != antlr.TokenEOF {
			ts.Consume()
		}
	})
	defer releasePredictionState()
	ctx = p.SqlInvokedFunction()
	if len(listener.errs) > 0 {
		return nil, buildSyntaxError(listener.errs[0])
	}
	return ctx, nil
}

// ParseView parses a view definition (the body of CREATE VIEW
// foo AS ...) and returns the query parse tree. Mirrors Java's
// QueryParser.parseView. Any ANTLR-internal panic on adversarial
// token streams is converted to a clean ErrCodeSyntaxError.
func ParseView(sql string) (ctx antlrgen.IQueryContext, err error) {
	defer func() {
		if r := recover(); r != nil {
			ctx = nil
			err = &api.Error{
				Code:    api.ErrCodeSyntaxError,
				Message: fmt.Sprintf("parse view panic: %v", r),
			}
		}
	}()
	p, listener, releasePredictionState := newParser(sql, nil)
	defer releasePredictionState()
	ctx = p.Query()
	if len(listener.errs) > 0 {
		return nil, buildSyntaxError(listener.errs[0])
	}
	return ctx, nil
}

// ParseExpression parses a bare boolean/scalar expression fragment and
// returns its IExpressionContext. Used to synthesize a JOIN ON predicate
// from a `USING (col, ...)` clause: the column list is lowered to the
// equivalent ON text (`col = rhs.col AND ...`) and re-parsed here so all
// downstream ON consumers (map-eval, cascades predicate resolution,
// explain) treat USING exactly like ON. Any ANTLR-internal panic on
// adversarial input is converted to a clean ErrCodeSyntaxError.
func ParseExpression(sql string) (ctx antlrgen.IExpressionContext, err error) {
	defer func() {
		if r := recover(); r != nil {
			ctx = nil
			err = &api.Error{
				Code:    api.ErrCodeSyntaxError,
				Message: fmt.Sprintf("parse expression panic: %v", r),
			}
		}
	}()
	p, listener, releasePredictionState := newParser(sql, nil)
	defer releasePredictionState()
	ctx = p.Expression()
	if len(listener.errs) > 0 {
		return nil, buildSyntaxError(listener.errs[0])
	}
	return ctx, nil
}

// ValidateNoPreparedParams walks a parse tree and returns an error
// if any ? / $N prepared-statement placeholder is present. Mirrors
// Java's QueryParser.validateNoPreparedParams — certain contexts
// (function bodies, view definitions) reject parameters because
// binding happens at parse time, not execution time.
func ValidateNoPreparedParams(tree antlr.ParseTree) error {
	v := &noPreparedParamsListener{}
	antlr.ParseTreeWalkerDefault.Walk(v, tree)
	if v.found {
		return api.NewError(api.ErrCodeSyntaxError, "found prepared parameter(s) in SQL statement")
	}
	return nil
}

// newParser wires up the full lexer + parser stack with our
// collecting error listener. tokenHook, if non-nil, runs on the
// token stream before the parser starts — mirrors Java's
// tokenConsumer hook (used by ParseFunction to skip CREATE).
func newParser(
	sql string,
	tokenHook func(*antlr.CommonTokenStream),
) (*antlrgen.RelationalParser, *collectingErrorListener, func()) {
	input := newCaseInsensitiveCharStream(sql)
	lexer := antlrgen.NewRelationalLexer(input)
	var (
		p           *antlrgen.RelationalParser
		lexerState  *predictionState
		parserState *predictionState
	)
	releasePredictionState := func() {
		if parserState != nil {
			// Generated contexts retain their parser. Detach it from the
			// warmed lease before pooling so a returned tree can be walked (or
			// inspected through GetParser) while a later parse reuses the
			// lease.
			detachParserPredictionState(p, parserState.atn)
			relationalParserPredictionStates.release(parserState)
			parserState = nil
		}
		if lexerState != nil {
			// The parser's token stream likewise retains its lexer.
			detachLexerPredictionState(lexer, lexerState.atn)
			relationalLexerPredictionStates.release(lexerState)
			lexerState = nil
		}
	}
	constructed := false
	defer func() {
		if !constructed {
			releasePredictionState()
		}
	}()
	lexerState = relationalLexerPredictionStates.acquire(lexer.Interpreter.ATN())
	useLexerPredictionState(lexer, lexerState)

	listener := &collectingErrorListener{sql: sql}
	lexer.RemoveErrorListeners()
	lexer.AddErrorListener(listener)

	tokens := antlr.NewCommonTokenStream(lexer, antlr.TokenDefaultChannel)
	if tokenHook != nil {
		tokenHook(tokens)
	}

	p = antlrgen.NewRelationalParser(tokens)
	parserState = relationalParserPredictionStates.acquire(p.Interpreter.ATN())
	useParserPredictionState(p, parserState)
	p.RemoveErrorListeners()
	p.AddErrorListener(listener)
	constructed = true
	return p, listener, releasePredictionState
}

// ANTLR's generated constructors reuse package-global DFA slices and
// prediction-context caches. Those objects are populated as input is lexed and
// parsed. Lease each Parse call an exclusive state bundle so parallel parses
// never share mutable prediction state. Returning completed bundles to pools
// preserves their warmed DFAs for later sequential parses without serializing
// the public Parse API.
type predictionState struct {
	atn                *antlr.ATN
	decisionToDFA      []*antlr.DFA
	sharedContextCache *antlr.PredictionContextCache
}

type predictionStatePool struct {
	mu        sync.Mutex
	available []*predictionState
}

// A warmed grammar state can retain multiple megabytes. Keep enough leases for
// normal CPU-parallel parsing, but do not retain an unbounded burst forever.
// The mutex covers only free-list checkout/return, never lexing or parsing.
var maxRetainedPredictionStates = min(runtime.GOMAXPROCS(0), 8)

var (
	relationalLexerPredictionStates  predictionStatePool
	relationalParserPredictionStates predictionStatePool
)

func (pool *predictionStatePool) acquire(atn *antlr.ATN) *predictionState {
	pool.mu.Lock()
	for len(pool.available) > 0 {
		last := len(pool.available) - 1
		state := pool.available[last]
		pool.available = pool.available[:last]
		if state.atn == atn {
			pool.mu.Unlock()
			return state
		}
	}
	pool.mu.Unlock()
	return newPredictionState(atn)
}

func newPredictionState(atn *antlr.ATN) *predictionState {
	return &predictionState{
		atn:                atn,
		decisionToDFA:      freshDecisionToDFA(atn),
		sharedContextCache: antlr.NewPredictionContextCache(),
	}
}

func (pool *predictionStatePool) release(state *predictionState) {
	pool.mu.Lock()
	if len(pool.available) < maxRetainedPredictionStates {
		pool.available = append(pool.available, state)
	}
	pool.mu.Unlock()
}

func useLexerPredictionState(lexer *antlrgen.RelationalLexer, state *predictionState) {
	lexer.Interpreter = antlr.NewLexerATNSimulator(
		lexer,
		state.atn,
		state.decisionToDFA,
		state.sharedContextCache,
	)
}

func useParserPredictionState(p *antlrgen.RelationalParser, state *predictionState) {
	p.Interpreter = antlr.NewParserATNSimulator(
		p,
		state.atn,
		state.decisionToDFA,
		state.sharedContextCache,
	)
}

func detachLexerPredictionState(lexer *antlrgen.RelationalLexer, atn *antlr.ATN) {
	lexer.Interpreter = antlr.NewLexerATNSimulator(lexer, atn, nil, nil)
}

func detachParserPredictionState(p *antlrgen.RelationalParser, atn *antlr.ATN) {
	p.Interpreter = antlr.NewParserATNSimulator(p, atn, nil, nil)
}

func freshDecisionToDFA(atn *antlr.ATN) []*antlr.DFA {
	decisionToDFA := make([]*antlr.DFA, len(atn.DecisionToState))
	for decision, state := range atn.DecisionToState {
		decisionToDFA[decision] = antlr.NewDFA(state, decision)
	}
	return decisionToDFA
}

// noPreparedParamsListener walks a parse tree and flips `found` on the
// first PreparedStatementParameter node it sees. Listener rather than
// visitor so we don't need type-specific code — the node name is all
// we need.
type noPreparedParamsListener struct {
	*antlr.BaseParseTreeListener
	found bool
}

func (v *noPreparedParamsListener) EnterEveryRule(ctx antlr.ParserRuleContext) {
	if _, ok := ctx.(antlrgen.IPreparedStatementParameterContext); ok {
		v.found = true
	}
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
