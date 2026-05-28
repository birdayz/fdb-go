package embedded

import (
	"testing"
)

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
		{"mixed comments", "SELECT /* block */ * FROM foo -- line", "SELECT * FROM FOO"},
		{"empty", "", ""},
		{"only whitespace", "   \t\n  ", ""},
		{"only comment", "-- just a comment", ""},
		{"case insensitive", "SeLeCt * FrOm FoO", "SELECT * FROM FOO"},
		{"string literal preserved", "SELECT 'hello' FROM foo", "SELECT 'hello' FROM FOO"},
		{"comment inside string literal", "SELECT '--not a comment' FROM foo", "SELECT '--not a comment' FROM FOO"},
		{"block comment inside string", "SELECT '/*not a comment*/' FROM foo", "SELECT '/*not a comment*/' FROM FOO"},
		{"escaped quote in literal", "SELECT 'it''s' FROM foo", "SELECT 'it''s' FROM FOO"},
		{"unterminated block comment", "SELECT * FROM foo /* unterminated", "SELECT * FROM FOO D"},
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

func TestNormalizeSQL_Determinism(t *testing.T) {
	t.Parallel()
	sql := "SELECT * FROM foo WHERE id = 1"
	n1 := normalizeSQL(sql)
	n2 := normalizeSQL(sql)
	if n1 != n2 {
		t.Fatalf("non-deterministic: %q vs %q", n1, n2)
	}
}

func TestNormalizeSQL_EquivalenceClasses(t *testing.T) {
	t.Parallel()
	groups := [][]string{
		{
			"SELECT * FROM foo",
			"select * from foo",
			"SELECT  *  FROM  foo",
			"  SELECT * FROM foo  ",
			"\t\tSELECT * FROM foo\n\n",
			"SELECT\t*\tFROM\tfoo",
			"SELECT\n*\nFROM\nfoo",
			"SELECT * FROM foo -- comment",
			"SELECT /* block */ * FROM foo",
		},
	}
	for _, group := range groups {
		canonical := normalizeSQL(group[0])
		for _, variant := range group[1:] {
			got := normalizeSQL(variant)
			if got != canonical {
				t.Errorf("normalizeSQL(%q) = %q, want %q (same as %q)", variant, got, canonical, group[0])
			}
		}
	}
}

func TestNormalizeSQL_DifferentQueriesDistinct(t *testing.T) {
	t.Parallel()
	n1 := normalizeSQL("SELECT * FROM foo")
	n2 := normalizeSQL("SELECT * FROM bar")
	if n1 == n2 {
		t.Fatal("different queries normalized to the same string")
	}
}
