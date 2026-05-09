package embedded

import (
	"testing"
)

func TestQueryHash_SameQuery(t *testing.T) {
	t.Parallel()
	sql := "SELECT * FROM foo WHERE id = 1"
	h1 := QueryHash(sql)
	h2 := QueryHash(sql)
	if h1 != h2 {
		t.Fatalf("same query produced different hashes: %d vs %d", h1, h2)
	}
}

func TestQueryHash_CaseInsensitive(t *testing.T) {
	t.Parallel()
	h1 := QueryHash("SELECT * FROM foo")
	h2 := QueryHash("select * from foo")
	h3 := QueryHash("SeLeCt * FrOm FoO")
	if h1 != h2 || h2 != h3 {
		t.Fatalf("case differences produced different hashes: %d, %d, %d", h1, h2, h3)
	}
}

func TestQueryHash_WhitespaceNormalization(t *testing.T) {
	t.Parallel()
	h1 := QueryHash("SELECT * FROM foo")
	h2 := QueryHash("SELECT  *  FROM  foo")
	h3 := QueryHash("SELECT   *   FROM   foo")
	if h1 != h2 || h2 != h3 {
		t.Fatalf("whitespace differences produced different hashes: %d, %d, %d", h1, h2, h3)
	}
}

func TestQueryHash_CommentStripping(t *testing.T) {
	t.Parallel()

	base := QueryHash("SELECT * FROM foo")

	// Single-line comment
	h1 := QueryHash("SELECT * FROM foo -- this is a comment")
	if h1 != base {
		t.Fatalf("single-line comment changed hash: %d vs %d", h1, base)
	}

	// Block comment
	h2 := QueryHash("SELECT /* a comment */ * FROM foo")
	if h2 != base {
		t.Fatalf("block comment changed hash: %d vs %d", h2, base)
	}

	// Both comment types
	h3 := QueryHash("SELECT /* block */ * FROM foo -- line")
	if h3 != base {
		t.Fatalf("mixed comments changed hash: %d vs %d", h3, base)
	}
}

func TestQueryHash_DifferentQueries(t *testing.T) {
	t.Parallel()
	h1 := QueryHash("SELECT * FROM foo")
	h2 := QueryHash("SELECT * FROM bar")
	if h1 == h2 {
		t.Fatal("different queries produced the same hash")
	}
}

func TestQueryHash_EmptyString(t *testing.T) {
	t.Parallel()
	h1 := QueryHash("")
	h2 := QueryHash("")
	if h1 != h2 {
		t.Fatalf("empty string produced different hashes: %d vs %d", h1, h2)
	}
	// FNV-64a of empty input is the offset basis — just verify determinism.
}

func TestQueryHash_LeadingTrailingWhitespace(t *testing.T) {
	t.Parallel()
	h1 := QueryHash("SELECT * FROM foo")
	h2 := QueryHash("  SELECT * FROM foo  ")
	h3 := QueryHash("\t\tSELECT * FROM foo\n\n")
	if h1 != h2 || h2 != h3 {
		t.Fatalf("leading/trailing whitespace changed hash: %d, %d, %d", h1, h2, h3)
	}
}

func TestQueryHash_TabNewline(t *testing.T) {
	t.Parallel()
	h1 := QueryHash("SELECT * FROM foo")
	h2 := QueryHash("SELECT\t*\tFROM\tfoo")
	h3 := QueryHash("SELECT\n*\nFROM\nfoo")
	if h1 != h2 || h2 != h3 {
		t.Fatalf("tabs/newlines produced different hashes: %d, %d, %d", h1, h2, h3)
	}
}

func TestQueryHash_StringLiteralPreserved(t *testing.T) {
	t.Parallel()
	// Comments inside string literals should NOT be stripped.
	h1 := QueryHash("SELECT '--not a comment' FROM foo")
	h2 := QueryHash("SELECT '/*not a comment*/' FROM foo")
	if h1 == h2 {
		t.Fatal("different string literals produced the same hash")
	}
}

func TestQueryHash_UnterminatedBlockComment(t *testing.T) {
	t.Parallel()
	// Should not panic on malformed input.
	_ = QueryHash("SELECT * FROM foo /* unterminated")
}

func TestNormalizeSQL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"simple", "select * from foo", "SELECT * FROM FOO"},
		{"extra spaces", "  select  *  from  foo  ", "SELECT * FROM FOO"},
		{"tabs", "select\t*\tfrom\tfoo", "SELECT * FROM FOO"},
		{"newlines", "select\n*\nfrom\nfoo", "SELECT * FROM FOO"},
		{"line comment", "select * from foo -- comment", "SELECT * FROM FOO"},
		{"block comment", "select /* x */ * from foo", "SELECT * FROM FOO"},
		{"empty", "", ""},
		{"only whitespace", "   \t\n  ", ""},
		{"only comment", "-- just a comment", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := normalizeSQL(tt.input)
			if got != tt.expected {
				t.Errorf("normalizeSQL(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}
