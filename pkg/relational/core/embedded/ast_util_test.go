package embedded

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/functions"
)

// TestStripIdentifierQuotes pins the contract for unwrapping
// double-quoted or backtick-quoted identifiers. Both forms strip
// matching pairs only (mismatched / unmatched / empty / single-char
// inputs pass through). The helper is the canonical step before
// case-folding and dictionary lookup, so a regression here would
// surface as ambiguous-column / undefined-column SQL errors at
// runtime.
func TestStripIdentifierQuotes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want string
	}{
		{`"hello"`, "hello"},
		{"`hello`", "hello"},
		{"plain", "plain"},
		{"", ""},
		{`"a"`, "a"},
		// Mismatched pairs pass through unchanged.
		{`"hello`, `"hello`},
		{`hello"`, `hello"`},
		{"`hello", "`hello"},
		// Single character: no quote wrap possible.
		{`"`, `"`},
		{"`", "`"},
		// Mixed quote styles aren't a pair.
		{"`hello\"", "`hello\""},
		{"\"hello`", "\"hello`"},
		// Internal quotes are preserved (no unescaping at this layer).
		{`"a"b"`, `a"b`},
		// Backtick variant similarly.
		{"`a`b`", "a`b"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			if got := functions.StripIdentifierQuotes(tc.in); got != tc.want {
				t.Errorf("StripIdentifierQuotes(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestStripStringLiteralQuotes pins the contract for unwrapping SQL
// string literals: removes one pair of surrounding single-quotes and
// unescapes doubled-quote `”` to `'` (SQL standard apostrophe-escape
// inside string literals). Helper is invoked at every LIKE pattern,
// CAST, and INSERT VALUES literal site, so a regression here would
// surface as either over-quoted stored values or apostrophe parse
// errors.
func TestStripStringLiteralQuotes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want string
	}{
		// Bare quoted string.
		{`'hello'`, "hello"},
		{`''`, ""},
		// Apostrophe escape: '' inside a quoted string → single '.
		{`'it''s fine'`, "it's fine"},
		// Multiple escapes.
		{`'a''b''c'`, "a'b'c"},
		// Unquoted (no surrounding quotes): pass through, but doubled
		// apostrophes still unescape.
		{`hello`, "hello"},
		{`a''b`, "a'b"},
		// Mismatched / unbalanced single-quote: pass through (the strip
		// step requires both ends).
		{`'hello`, "'hello"},
		{`hello'`, "hello'"},
		// Single character can't form a quote pair.
		{`'`, "'"},
		// Quote-only inside: stripping outer pair leaves '' which
		// unescapes to '.
		{`''''`, "'"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			if got := functions.StripStringLiteralQuotes(tc.in); got != tc.want {
				t.Errorf("StripStringLiteralQuotes(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
