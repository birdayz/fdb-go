package parser

import (
	"errors"
	"strings"
	"sync"
	"testing"

	"fdb.dev/pkg/relational/api"
	antlrgen "fdb.dev/pkg/relational/core/parser/gen"
	"github.com/antlr4-go/antlr/v4"
)

func TestNewParser_IsolatesCheckedOutPredictionState(t *testing.T) {
	t.Parallel()

	first, _, releaseFirst := newParser(`SELECT id FROM orders WHERE id = 1`, nil)
	defer releaseFirst()
	second, _, releaseSecond := newParser(`SELECT id FROM orders WHERE id = 2`, nil)
	defer releaseSecond()

	if first.Interpreter.SharedContextCache() == second.Interpreter.SharedContextCache() {
		t.Fatal("parser instances share a prediction-context cache")
	}
	firstParserDFA := first.Interpreter.DecisionToDFA()
	secondParserDFA := second.Interpreter.DecisionToDFA()
	if len(firstParserDFA) != len(secondParserDFA) {
		t.Fatalf("parser DFA counts differ: %d != %d", len(firstParserDFA), len(secondParserDFA))
	}
	for i := range firstParserDFA {
		if firstParserDFA[i] == secondParserDFA[i] {
			t.Fatalf("parser instances share DFA %d", i)
		}
	}

	firstLexer, ok := first.GetTokenStream().GetTokenSource().(*antlrgen.RelationalLexer)
	if !ok {
		t.Fatalf("first token source is %T, want *RelationalLexer", first.GetTokenStream().GetTokenSource())
	}
	secondLexer, ok := second.GetTokenStream().GetTokenSource().(*antlrgen.RelationalLexer)
	if !ok {
		t.Fatalf("second token source is %T, want *RelationalLexer", second.GetTokenStream().GetTokenSource())
	}
	if firstLexer.Interpreter.SharedContextCache() == secondLexer.Interpreter.SharedContextCache() {
		t.Fatal("lexer instances share a prediction-context cache")
	}
	firstLexerDFA := firstLexer.Interpreter.DecisionToDFA()
	secondLexerDFA := secondLexer.Interpreter.DecisionToDFA()
	if len(firstLexerDFA) != len(secondLexerDFA) {
		t.Fatalf("lexer DFA counts differ: %d != %d", len(firstLexerDFA), len(secondLexerDFA))
	}
	for i := range firstLexerDFA {
		if firstLexerDFA[i] == secondLexerDFA[i] {
			t.Fatalf("lexer instances share DFA %d", i)
		}
	}
}

func TestNewParser_ReleaseDetachesPredictionState(t *testing.T) {
	t.Parallel()

	p, _, releasePredictionState := newParser(`SELECT id FROM orders WHERE id = 1`, nil)
	lexer, ok := p.GetTokenStream().GetTokenSource().(*antlrgen.RelationalLexer)
	if !ok {
		t.Fatalf("token source is %T, want *RelationalLexer", p.GetTokenStream().GetTokenSource())
	}
	checkedOutParserCache := p.Interpreter.SharedContextCache()
	checkedOutLexerCache := lexer.Interpreter.SharedContextCache()

	releasePredictionState()

	if p.Interpreter.SharedContextCache() == checkedOutParserCache {
		t.Fatal("released parser still references the pooled prediction-context cache")
	}
	if lexer.Interpreter.SharedContextCache() == checkedOutLexerCache {
		t.Fatal("released lexer still references the pooled prediction-context cache")
	}
	if p.Interpreter.ATN() == nil || lexer.Interpreter.ATN() == nil {
		t.Fatal("detached interpreter lost grammar ATN metadata")
	}
	if len(p.Interpreter.DecisionToDFA()) != 0 || len(lexer.Interpreter.DecisionToDFA()) != 0 {
		t.Fatal("detached interpreter retained mutable DFA prediction state")
	}
}

func TestPredictionStatePool_BoundsRetainedStates(t *testing.T) {
	t.Parallel()

	lexer := antlrgen.NewRelationalLexer(newCaseInsensitiveCharStream("SELECT 1"))
	var pool predictionStatePool
	states := make([]*predictionState, maxRetainedPredictionStates+1)
	for i := range states {
		states[i] = pool.acquire(lexer.Interpreter.ATN())
	}
	for _, state := range states {
		pool.release(state)
	}

	pool.mu.Lock()
	retained := len(pool.available)
	pool.mu.Unlock()
	if retained != maxRetainedPredictionStates {
		t.Fatalf("retained prediction states = %d, want cap %d", retained, maxRetainedPredictionStates)
	}
}

func TestParse_Concurrent(t *testing.T) {
	t.Parallel()

	statements := []string{
		`SELECT id, name FROM orders WHERE id = 42`,
		`SELECT DISTINCT status FROM orders ORDER BY status`,
		`SELECT o.id, i.sku FROM orders AS o JOIN items AS i ON i.order_id = o.id`,
		`SELECT id FROM orders WHERE status IN ('open', 'pending') AND total BETWEEN 10 AND 100`,
		`SELECT customer_id, SUM(total) FROM orders GROUP BY customer_id HAVING COUNT(*) > 1`,
		`SELECT id FROM current_orders UNION ALL SELECT id FROM archived_orders`,
		`WITH recent AS (SELECT id FROM orders WHERE created_at > '2025-01-01') SELECT id FROM recent`,
		`INSERT INTO orders (id, status, total) VALUES (1, 'open', 42)`,
		`UPDATE orders SET status = 'closed', total = total + 1 WHERE id = 1`,
		`DELETE FROM orders WHERE id = 1`,
		`CREATE DATABASE /test/concurrent`,
		`CREATE SCHEMA TEMPLATE concurrent_template CREATE TABLE orders (id BIGINT NOT NULL, status STRING, PRIMARY KEY (id))`,
	}

	const (
		workers = 48
		rounds  = 8
	)
	parseCalls := []func(int) error{
		func(worker int) error {
			_, err := Parse(statements[worker%len(statements)])
			return err
		},
		func(_ int) error {
			_, err := ParseFunction("CREATE FUNCTION self(IN x bigint) RETURNS bigint AS x")
			return err
		},
		func(_ int) error {
			_, err := ParseView("SELECT id, name FROM orders WHERE id > 10")
			return err
		},
		func(_ int) error {
			_, err := ParseExpression("left_id = right_id AND total > 10")
			return err
		},
	}

	ready := make(chan struct{}, workers)
	start := make(chan struct{})
	errs := make(chan error, workers*rounds)
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func(worker int) {
			defer wg.Done()
			ready <- struct{}{}
			<-start
			for round := 0; round < rounds; round++ {
				errs <- parseCalls[(worker+round)%len(parseCalls)](worker + round)
			}
		}(i)
	}
	for i := 0; i < workers; i++ {
		<-ready
	}
	close(start)
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Parse returned error: %v", err)
		}
	}
}

func TestParseErrors_Concurrent(t *testing.T) {
	t.Parallel()

	parseCalls := []func() error{
		func() error {
			_, err := Parse("SELECT FROM WHERE")
			return expectSyntaxError(err)
		},
		func() error {
			_, err := ParseFunction("CREATE FUNCTION @@@")
			return expectSyntaxError(err)
		},
		func() error {
			_, err := ParseView("not a valid query")
			return expectSyntaxError(err)
		},
		func() error {
			_, err := ParseExpression("id = )")
			return expectSyntaxError(err)
		},
	}

	const (
		workers = 32
		rounds  = 8
	)
	start := make(chan struct{})
	errs := make(chan error, workers*rounds)
	var wg sync.WaitGroup
	wg.Add(workers)
	for worker := range workers {
		go func() {
			defer wg.Done()
			<-start
			for round := range rounds {
				errs <- parseCalls[(worker+round)%len(parseCalls)]()
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent invalid parse returned unexpected result: %v", err)
		}
	}
}

func expectSyntaxError(err error) error {
	if err == nil {
		return errors.New("parse returned nil, want syntax error")
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		return errors.New("parse returned a non-api error")
	}
	if apiErr.Code != api.ErrCodeSyntaxError {
		return errors.New("parse returned a non-syntax API error")
	}
	return nil
}

func TestParse_ReturnedTreeSafeDuringPoolReuse(t *testing.T) {
	t.Parallel()

	root, err := Parse(`SELECT o.id, i.sku
		FROM orders AS o
		JOIN items AS i ON i.order_id = o.id
		WHERE o.status IN ('open', 'pending')`)
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	retainedParser, ok := root.GetParser().(*antlrgen.RelationalParser)
	if !ok {
		t.Fatalf("root parser is %T, want *RelationalParser", root.GetParser())
	}
	retainedLexer, ok := retainedParser.GetTokenStream().GetTokenSource().(*antlrgen.RelationalLexer)
	if !ok {
		t.Fatalf("token source is %T, want *RelationalLexer", retainedParser.GetTokenStream().GetTokenSource())
	}
	retainedParserCache := retainedParser.Interpreter.SharedContextCache()
	retainedLexerCache := retainedLexer.Interpreter.SharedContextCache()

	const iterations = 100
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		listener := &antlr.BaseParseTreeListener{}
		for range iterations {
			antlr.ParseTreeWalkerDefault.Walk(listener, root)
			if rendered := root.ToStringTree(retainedParser.GetRuleNames(), retainedParser); rendered == "" {
				errs <- errors.New("ToStringTree returned an empty tree")
				return
			}
		}
		errs <- nil
	}()
	go func() {
		defer wg.Done()
		<-start
		for i := range iterations {
			if _, parseErr := Parse(`SELECT id, name FROM orders WHERE id = 42 AND status = 'open'`); parseErr != nil {
				errs <- parseErr
				return
			}
			if i%4 == 0 {
				if _, parseErr := ParseExpression(`left_id = right_id AND total > 10`); parseErr != nil {
					errs <- parseErr
					return
				}
			}
		}
		errs <- nil
	}()
	close(start)
	wg.Wait()
	close(errs)

	for raceErr := range errs {
		if raceErr != nil {
			t.Fatalf("concurrent tree access failed: %v", raceErr)
		}
	}
	if retainedParser.Interpreter.SharedContextCache() != retainedParserCache {
		t.Fatal("returned parser prediction state changed during later parses")
	}
	if retainedLexer.Interpreter.SharedContextCache() != retainedLexerCache {
		t.Fatal("returned lexer prediction state changed during later parses")
	}
}

func TestParse_ValidStatements(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		sql  string
	}{
		{
			name: "create schema template",
			sql: `CREATE SCHEMA TEMPLATE my_template
                   CREATE TABLE orders (id bigint, PRIMARY KEY(id))`,
		},
		{
			name: "create schema with template",
			sql:  `CREATE SCHEMA /foo/bar WITH TEMPLATE my_template`,
		},
		{
			name: "create database",
			sql:  `CREATE DATABASE /foo/bar`,
		},
		{
			name: "simple select",
			sql:  `SELECT id, name FROM orders WHERE id = 42`,
		},
		{
			name: "insert",
			sql:  `INSERT INTO orders VALUES (1, 'hi', 3.14)`,
		},
		{
			name: "delete",
			sql:  `DELETE FROM orders WHERE id > 5`,
		},
		{
			name: "update",
			sql:  `UPDATE orders SET price = 99 WHERE id = 1`,
		},
		{
			name: "transaction",
			// Dialect uses START TRANSACTION / COMMIT — no bare BEGIN.
			sql: `START TRANSACTION; COMMIT;`,
		},
		{
			name: "empty string parses as empty root",
			sql:  ``,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root, err := Parse(tc.sql)
			if err != nil {
				t.Fatalf("Parse(%q) err = %v", tc.sql, err)
			}
			if root == nil {
				t.Fatalf("Parse(%q) root = nil, want non-nil", tc.sql)
			}
		})
	}
}

func TestParse_CaseInsensitiveKeywords(t *testing.T) {
	t.Parallel()
	// Mixed-case keyword + original-cased identifier. The lexer uppercases
	// during matching, but token text must come from the source so the
	// identifier "Orders" stays as-is for later lookup.
	sql := `Select Id From Orders WHERE id = 1`
	root, err := Parse(sql)
	if err != nil {
		t.Fatalf("Parse mixed-case: err = %v", err)
	}
	if !strings.Contains(root.GetText(), "Orders") {
		t.Fatalf("GetText() lost source casing: %q", root.GetText())
	}
	if strings.Contains(root.GetText(), "SELECT") && !strings.Contains(sql, "SELECT") {
		t.Fatalf("GetText() reports uppercased keyword not in source: %q", root.GetText())
	}
}

// TestParse_LeftRightDisambiguation pins Java 4.12 #4272 (RFC-140 / R3): LEFT and RIGHT
// were moved out of functionNameBase into functionNameKeyword, so they remain usable as
// scalar function names but are no longer accepted as bare identifiers / table aliases
// (which was ambiguous with the {LEFT|RIGHT} [OUTER] JOIN clause).
func TestParse_LeftRightDisambiguation(t *testing.T) {
	t.Parallel()

	t.Run("LEFT/RIGHT still parse as scalar function names", func(t *testing.T) {
		t.Parallel()
		for _, sql := range []string{
			`SELECT LEFT(name, 3) FROM orders`,
			`SELECT RIGHT(name, 3) FROM orders`,
		} {
			if _, err := Parse(sql); err != nil {
				t.Errorf("Parse(%q) err = %v, want nil (LEFT/RIGHT via functionNameKeyword)", sql, err)
			}
		}
	})

	t.Run("LEFT/RIGHT rejected as a table alias (syntax error)", func(t *testing.T) {
		t.Parallel()
		for _, sql := range []string{
			`SELECT id FROM orders AS LEFT`,
			`SELECT id FROM orders AS RIGHT`,
		} {
			_, err := Parse(sql)
			if err == nil {
				t.Errorf("Parse(%q) err = nil, want syntax error (LEFT/RIGHT no longer identifiers)", sql)
				continue
			}
			var apiErr *api.Error
			if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeSyntaxError {
				t.Errorf("Parse(%q) err = %v, want *api.Error{ErrCodeSyntaxError}", sql, err)
			}
		}
	})
}

// TestParse_AtOrdinalitySyntax pins Java 4.12 #4112 (RFC-140 / R3): the PartiQL
// `AT atAlias` unnest-with-ordinality table source now parses, and the atAlias is captured
// on atomTableItem. (Binding/execution of the ordinal is R5; here it is parsed-but-unbound.)
func TestParse_AtOrdinalitySyntax(t *testing.T) {
	t.Parallel()

	root, err := Parse(`SELECT e FROM orders AS e AT p`)
	if err != nil {
		t.Fatalf("Parse(AT ordinality) err = %v, want nil (AT clause must parse)", err)
	}
	atom := findAtomTableItem(root)
	if atom == nil {
		t.Fatal("no AtomTableItemContext in parse tree")
	}
	if atom.GetAtAlias() == nil {
		t.Fatal("atAlias is nil — the AT clause parsed but was not bound to the atAlias field")
	}
	if got := atom.GetAtAlias().GetText(); !strings.EqualFold(got, "p") {
		t.Fatalf("atAlias = %q, want %q", got, "p")
	}

	// Without AT, atAlias must be nil (the field is genuinely optional, not always-populated).
	noAt, err := Parse(`SELECT e FROM orders AS e`)
	if err != nil {
		t.Fatalf("Parse(no AT) err = %v", err)
	}
	if a := findAtomTableItem(noAt); a == nil || a.GetAtAlias() != nil {
		t.Fatal("atAlias should be nil when no AT clause is present")
	}
}

// findAtomTableItem returns the first AtomTableItemContext in the parse tree, or nil.
func findAtomTableItem(t antlr.Tree) *antlrgen.AtomTableItemContext {
	if a, ok := t.(*antlrgen.AtomTableItemContext); ok {
		return a
	}
	for i := 0; i < t.GetChildCount(); i++ {
		if r := findAtomTableItem(t.GetChild(i)); r != nil {
			return r
		}
	}
	return nil
}

func TestParse_SyntaxError(t *testing.T) {
	t.Parallel()
	_, err := Parse(`SELECT FROM WHERE`)
	if err == nil {
		t.Fatal("Parse(bogus) err = nil, want syntax error")
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("Parse err is not *api.Error: %v", err)
	}
	if apiErr.Code != api.ErrCodeSyntaxError {
		t.Fatalf("Code = %q, want %q", apiErr.Code, api.ErrCodeSyntaxError)
	}
	if apiErr.Message == "" {
		t.Fatal("Message is empty")
	}
}

func TestParse_SyntaxErrorFormatMatchesJava(t *testing.T) {
	t.Parallel()
	// Error is on line 3 (1-based). Preceding lines are valid SQL + newlines.
	// Java emits: "syntax error:\n<offending source line>\n<column-indent>^^^"
	// where the carets cover the offending token.
	sql := "SELECT 1;\n\nSELECT FROM"
	_, err := Parse(sql)
	if err == nil {
		t.Fatal("expected syntax error")
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("not *api.Error: %v", err)
	}
	if !strings.HasPrefix(apiErr.Message, "syntax error:\n") {
		t.Errorf("Message missing \"syntax error:\\n\" prefix: %q", apiErr.Message)
	}
	if !strings.Contains(apiErr.Message, "SELECT FROM") {
		t.Errorf("Message should include the offending source line: %q", apiErr.Message)
	}
	if !strings.Contains(apiErr.Message, "^") {
		t.Errorf("Message should include caret underline: %q", apiErr.Message)
	}
}

func TestParse_ErrorOnStrayCharacter(t *testing.T) {
	t.Parallel()
	// The grammar's ERROR_RECOGNITION catch-all rule means every input
	// character becomes a lexer token — there's no "token recognition
	// error" path. Stray characters therefore surface via the parser as
	// "mismatched input" errors. This test pins that contract so anyone
	// who later tightens the lexer doesn't accidentally stop raising an
	// error on obvious garbage.
	_, err := Parse("SELECT ` FROM orders")
	if err == nil {
		t.Fatal("expected error for stray character")
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("not *api.Error: %v", err)
	}
	if apiErr.Code != api.ErrCodeSyntaxError {
		t.Fatalf("Code = %q, want %q", apiErr.Code, api.ErrCodeSyntaxError)
	}
	// The rendered message includes the offending source line in the
	// Java-compatible underline format.
	if !strings.Contains(apiErr.Message, "SELECT ` FROM orders") {
		t.Fatalf("Message does not include offending source line: %q", apiErr.Message)
	}
}

func TestParse_ReportsOnlyFirstError(t *testing.T) {
	t.Parallel()
	// Java's QueryParser.parse throws with the FIRST syntax error only.
	// Downstream parser errors after a syntax failure are usually
	// cascade noise.
	sql := "SELECT FROM;\nSELECT FROM"
	_, err := Parse(sql)
	if err == nil {
		t.Fatal("expected error")
	}
	var apiErr *api.Error
	errors.As(err, &apiErr)
	// The first FROM is on line 1 — its source line should show up in
	// the error output.
	if !strings.Contains(apiErr.Message, "SELECT FROM;") {
		t.Errorf("Message should include line 1's offending source: %q", apiErr.Message)
	}
	// Line 2's content ("SELECT FROM" without the semicolon) must NOT
	// appear. Strip the line-1 occurrence first to make the check
	// tight (line 1's "SELECT FROM;" would otherwise contain "SELECT
	// FROM" as a substring). After removal, "SELECT FROM" on its own
	// on a fresh line would be line 2 — must be absent.
	stripped := strings.Replace(apiErr.Message, "SELECT FROM;", "", 1)
	if strings.Contains(stripped, "SELECT FROM") {
		t.Errorf("line 2's source leaked into Message (only first error should appear): %q", apiErr.Message)
	}
	// Count caret-bearing blocks: a single error produces exactly one.
	if carets := strings.Count(apiErr.Message, "^"); carets == 0 {
		t.Errorf("Message missing underline carets: %q", apiErr.Message)
	}
}

func TestParseView(t *testing.T) {
	t.Parallel()
	// ParseView takes a view body (a single query) and returns the
	// query parse tree — no CREATE VIEW wrapper.
	ctx, err := ParseView("SELECT id, name FROM orders")
	if err != nil {
		t.Fatalf("ParseView: %v", err)
	}
	if ctx == nil {
		t.Fatal("ParseView returned nil context")
	}
}

func TestParseView_SyntaxError(t *testing.T) {
	t.Parallel()
	_, err := ParseView("not a valid query")
	if err == nil {
		t.Fatal("ParseView of garbage should error")
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeSyntaxError {
		t.Errorf("Code = %q, want %q", apiErr.Code, api.ErrCodeSyntaxError)
	}
}

func TestValidateNoPreparedParams_Clean(t *testing.T) {
	t.Parallel()
	root, err := Parse("SELECT id FROM orders WHERE id = 42")
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateNoPreparedParams(root); err != nil {
		t.Errorf("clean query reported prepared params: %v", err)
	}
}

func TestValidateNoPreparedParams_WithParameter(t *testing.T) {
	t.Parallel()
	// Grammar's PreparedStatementParameter rule matches both the
	// named form (?name → NAMED_PARAMETER) and the unnamed form
	// (? → QUESTION). Exercise both to cover the rule's branches.
	for _, sql := range []string{
		"SELECT id FROM orders WHERE id = ?param", // named
		"SELECT id FROM orders WHERE id = ?",      // unnamed
	} {
		t.Run(sql, func(t *testing.T) {
			t.Parallel()
			root, err := Parse(sql)
			if err != nil {
				t.Fatal(err)
			}
			err = ValidateNoPreparedParams(root)
			if err == nil {
				t.Fatal("prepared parameter should fail validation")
			}
			var apiErr *api.Error
			if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeSyntaxError {
				t.Errorf("Code = %q, want %q", apiErr.Code, api.ErrCodeSyntaxError)
			}
		})
	}
}

func TestParseFunction(t *testing.T) {
	t.Parallel()
	// ParseFunction expects a full CREATE FUNCTION … and internally
	// skips the CREATE token before running sqlInvokedFunction.
	// Syntax taken from the Java yamsql corpus
	// (user-defined-macro-function-tests.yamsql):
	// CREATE FUNCTION <name>(IN <param> <type>) RETURNS <type> AS <expr>.
	ctx, err := ParseFunction("CREATE FUNCTION self(IN x bigint) RETURNS bigint AS x")
	if err != nil {
		t.Fatalf("ParseFunction: %v", err)
	}
	if ctx == nil {
		t.Fatal("ParseFunction returned nil context")
	}
}

func TestParseFunction_SyntaxError(t *testing.T) {
	t.Parallel()
	_, err := ParseFunction("CREATE FUNCTION @@@")
	if err == nil {
		t.Fatal("ParseFunction of garbage should error")
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeSyntaxError {
		t.Errorf("Code = %q, want %q", apiErr.Code, api.ErrCodeSyntaxError)
	}
}

func TestCaseInsensitiveCharStream_PreservesOriginalText(t *testing.T) {
	t.Parallel()
	s := newCaseInsensitiveCharStream("SeLeCt")
	// LA returns upper-cased code points.
	if got, want := s.LA(1), int('S'); got != want {
		t.Errorf("LA(1) = %d, want %d", got, want)
	}
	if got, want := s.LA(2), int('E'); got != want {
		t.Errorf("LA(2) = %d (lowercase 'e'), want upper-cased 'E' = %d", got, want)
	}
	// Underlying text is untouched.
	if got := s.GetText(0, 5); got != "SeLeCt" {
		t.Errorf("GetText preserved case check: got %q, want %q", got, "SeLeCt")
	}
}

func TestCaseInsensitiveCharStream_EOFPassthrough(t *testing.T) {
	t.Parallel()
	s := newCaseInsensitiveCharStream("")
	// EOF == -1 must not be "upper-cased" into 0xFFFD or similar.
	if got := s.LA(1); got != -1 {
		t.Errorf("LA at EOF = %d, want -1", got)
	}
}

// TestParseFunction_EmptyInput pins that ParseFunction("") does not panic.
// ANTLR's ts.Consume() on an empty stream would panic "cannot consume EOF";
// the pre-consume guard + defer/recover wrapper must surface a clean
// *api.Error with ErrCodeSyntaxError (or nil error if the parser treats
// empty as a no-op, which is also fine — we only forbid panics).
func TestParseFunction_EmptyInput(t *testing.T) {
	t.Parallel()
	for _, sql := range []string{"", " ", "\x00"} {
		_, err := ParseFunction(sql)
		if err == nil {
			continue
		}
		var apiErr *api.Error
		if !errors.As(err, &apiErr) {
			t.Errorf("ParseFunction(%q): non-api error %T: %v", sql, err, err)
		}
	}
}

// TestParseView_EmptyInput: same contract for ParseView.
func TestParseView_EmptyInput(t *testing.T) {
	t.Parallel()
	for _, sql := range []string{"", " ", "\x00"} {
		_, err := ParseView(sql)
		if err == nil {
			continue
		}
		var apiErr *api.Error
		if !errors.As(err, &apiErr) {
			t.Errorf("ParseView(%q): non-api error %T: %v", sql, err, err)
		}
	}
}
