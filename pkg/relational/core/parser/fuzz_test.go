package parser

import (
	"errors"
	"strings"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
)

// FuzzParse ensures that the public Parse function never panics on any
// byte sequence. Every failure must be a clean *api.Error with
// ErrCodeSyntaxError; anything else — a panic, a wrapped non-api error, or
// a nil-error-but-nil-result combination — is a bug.
//
// Seed corpus mixes well-formed SQL (to exercise the full grammar) with
// pathological edge cases that historically tripped ANTLR generators
// (unclosed string, leading NUL, high-bit bytes, control characters).
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
