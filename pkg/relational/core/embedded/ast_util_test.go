package embedded

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/functions"
)

// TestStripIdentifierQuotes pins the SQL identifier normalization
// contract: quoted forms (`"…"` or backticks) strip the surrounding
// pair and preserve case; unquoted forms fold to upper case (mirrors
// Java SemanticAnalyzer.normalizeString with case-sensitive=false,
// the default). Mismatched / unmatched / empty / single-char inputs
// fold (with no quote-strip) since they're treated as unquoted
// identifier text. The helper is the canonical step before catalog
// lookup, so a regression here would surface as ambiguous-column or
// undefined-column SQL errors at runtime.
func TestStripIdentifierQuotes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want string
	}{
		{`"hello"`, "hello"},
		{"`hello`", "hello"},
		{"plain", "PLAIN"},
		{"", ""},
		{`"a"`, "a"},
		// Mismatched pairs are unquoted text — fold to upper.
		{`"hello`, `"HELLO`},
		{`hello"`, `HELLO"`},
		{"`hello", "`HELLO"},
		// Single character: no quote wrap possible — folds to upper.
		{`"`, `"`},
		{"`", "`"},
		// Mixed quote styles aren't a pair — treated as unquoted, fold.
		{"`hello\"", "`HELLO\""},
		{"\"hello`", "\"HELLO`"},
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
