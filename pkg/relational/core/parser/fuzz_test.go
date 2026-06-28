package parser

import (
	"errors"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/api"
)

// FuzzParse ensures that the public Parse function never panics on any
// byte sequence. Every failure must be a clean *api.Error with
// ErrCodeSyntaxError; anything else — a panic, a wrapped non-api error, or
// a nil-error-but-nil-result combination — is a bug.
//
// Seed corpus mixes well-formed SQL (to exercise the full grammar) with
// pathological edge cases that historically tripped ANTLR generators
// (unclosed string, leading NUL, high-bit bytes, control characters).
//
// The testdata/fuzz/FuzzParse corpus also holds regression entries from
// prior fuzz runs. Notable: `a1c9802306691af3` — 3.4KB `CASE WHEN x IS
// NULL T((((...((HEN 'a' ELSE 'b' END FROM t` input that takes ~8.7s
// to parse due to exponential ANTLR ATN behaviour on deeply-unclosed
// parens. Parsed correctly (returns syntax error), just slow. Same
// grammar as Java so Java has the same vulnerability — deferring
// upstream rather than adding a Go-only size limit.
func FuzzParse(f *testing.F) {
	seeds := []string{
		"",
		" ",
		";",
		"SELECT 1",
		"SELECT * FROM t",
		"SELECT * FROM t WHERE x = 1",
		"CREATE TABLE t (id BIGINT NOT NULL, PRIMARY KEY (id))",
		"CREATE SCHEMA TEMPLATE foo CREATE TABLE t (id BIGINT NOT NULL, PRIMARY KEY (id))",
		"SELECT n FROM t WHERE n IN (1, 2, NULL)",
		"WITH C AS (SELECT 1) SELECT * FROM C",
		"SELECT CASE WHEN x IS NULL THEN 'a' ELSE 'b' END FROM t",
		"SELECT UPPER(s) || '!' FROM t",
		// Edge cases that shouldn't panic:
		"'",                            // unterminated string
		"\"",                           // unterminated identifier
		"-- comment without body",      // line comment at EOF
		"/*",                           // unterminated block comment
		"\x00SELECT",                   // leading NUL
		"SELECT\x00\x01",               // embedded control bytes
		"SELECT \xff\xfe",              // invalid UTF-8
		"(" + strings.Repeat("(", 100), // deep nesting
		";;;;",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, sql string) {
		// Any Parse call must either return a non-nil tree and nil error,
		// OR nil tree and an *api.Error with ErrCodeSyntaxError.
		root, err := Parse(sql)
		if err == nil {
			if root == nil {
				t.Fatalf("Parse(%q) returned (nil, nil) — one of root/err must be non-nil", sql)
			}
			return
		}
		// Error path: must be wrapped as *api.Error with syntax code.
		var apiErr *api.Error
		if !errors.As(err, &apiErr) {
			t.Fatalf("Parse(%q) returned non-api error %T: %v", sql, err, err)
		}
		if apiErr.Code != api.ErrCodeSyntaxError {
			t.Fatalf("Parse(%q) returned api error with unexpected code %s (want %s): %v",
				sql, apiErr.Code, api.ErrCodeSyntaxError, err)
		}
	})
}

// FuzzParseFunction and FuzzParseView mirror the FuzzParse invariant
// against the two other grammar entry points (CREATE FUNCTION body,
// CREATE VIEW body). Both use the same generated parser state machine
// so the same exponential-input classes apply — but the entry rule
// and token stream priming differ, so a separate fuzz run could surface
// entry-specific quirks.
func FuzzParseFunction(f *testing.F) {
	seeds := []string{
		"",
		"FUNCTION f() RETURNS INT AS BEGIN RETURN 1 END",
		"FUNCTION f(x INT) RETURNS INT AS BEGIN RETURN x END",
		"\x00",
		"(",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, sql string) {
		ctx, err := ParseFunction(sql)
		if err == nil {
			if ctx == nil {
				t.Fatalf("ParseFunction(%q) returned (nil, nil)", sql)
			}
			return
		}
		var apiErr *api.Error
		if !errors.As(err, &apiErr) {
			t.Fatalf("ParseFunction(%q) returned non-api error %T: %v", sql, err, err)
		}
		if apiErr.Code != api.ErrCodeSyntaxError {
			t.Fatalf("ParseFunction(%q) unexpected code %s: %v", sql, apiErr.Code, err)
		}
	})
}

func FuzzParseView(f *testing.F) {
	seeds := []string{
		"",
		"SELECT 1",
		"SELECT * FROM t",
		"(",
		"SELECT ,",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, sql string) {
		ctx, err := ParseView(sql)
		if err == nil {
			if ctx == nil {
				t.Fatalf("ParseView(%q) returned (nil, nil)", sql)
			}
			return
		}
		var apiErr *api.Error
		if !errors.As(err, &apiErr) {
			t.Fatalf("ParseView(%q) returned non-api error %T: %v", sql, err, err)
		}
		if apiErr.Code != api.ErrCodeSyntaxError {
			t.Fatalf("ParseView(%q) unexpected code %s: %v", sql, apiErr.Code, err)
		}
	})
}
