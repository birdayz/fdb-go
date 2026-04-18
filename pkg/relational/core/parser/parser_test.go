package parser

import (
	"errors"
	"strings"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
)

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

func TestParse_SyntaxErrorIncludesLineColumn(t *testing.T) {
	t.Parallel()
	// Error is on line 3 (1-based). Preceding lines are valid SQL + newlines.
	sql := "SELECT 1;\n\nSELECT FROM"
	_, err := Parse(sql)
	if err == nil {
		t.Fatal("expected syntax error")
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("not *api.Error: %v", err)
	}
	// Error lines are of the form "<line>:<col>: <msg>"
	if !strings.HasPrefix(apiErr.Message, "3:") {
		t.Fatalf("Message = %q, want 3:col prefix", apiErr.Message)
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
	if !strings.Contains(apiErr.Message, "`") {
		t.Fatalf("Message does not point at offending char: %q", apiErr.Message)
	}
}

func TestParse_ErrorListOrderStable(t *testing.T) {
	t.Parallel()
	// Two separate broken statements; both errors should appear, and the
	// line numbers should come out ascending.
	sql := "SELECT FROM;\nSELECT FROM"
	_, err := Parse(sql)
	if err == nil {
		t.Fatal("expected error")
	}
	var apiErr *api.Error
	errors.As(err, &apiErr)
	lines := strings.Split(apiErr.Message, "\n")
	if len(lines) < 2 {
		t.Fatalf("expected >=2 errors, got %d: %q", len(lines), apiErr.Message)
	}
	if !strings.HasPrefix(lines[0], "1:") {
		t.Fatalf("first error not on line 1: %q", lines[0])
	}
	if !strings.HasPrefix(lines[len(lines)-1], "2:") {
		t.Fatalf("last error not on line 2: %q", lines[len(lines)-1])
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
