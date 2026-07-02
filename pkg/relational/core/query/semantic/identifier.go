// Package semantic is the Go port of Java's
// `com.apple.foundationdb.relational.recordlayer.query.SemanticAnalyzer`
// plus related Identifier / Expression / reference-resolution helpers.
//
// The Java class is a 1280-line monolith doing identifier
// normalization, table / CTE / function lookup, index validation,
// ORDER BY validation, type inference, star expansion, nested-field
// resolution, and correlated-identifier resolution. The Go port
// breaks that up across focused files:
//
//   - identifier.go — Identifier type + case-folding normalization.
//   - (future)      — table / column / CTE / index lookup, type
//     inference, star expansion.
//
// Currently ships only the identifier machinery; the resolution +
// type passes are follow-up work.
package semantic

import "strings"

// Identifier is a SQL identifier — a table, column, alias, or
// function name. Carries the normalized form (case-folded per SQL
// rules) so Identifiers can live in maps and compare by `==`
// directly. The original source text is the caller's
// responsibility (store it alongside via the parse tree's token
// stream when you need user-spelling-preserving error messages).
//
// Mirrors Java's `com.apple.foundationdb.relational.recordlayer.
// query.Identifier`, trimmed to the essential equality surface.
type Identifier struct {
	// name is the normalized identifier text. Unquoted identifiers
	// are case-folded to upper-case (SQL standard); quoted
	// identifiers preserve case.
	name string
	// wasQuoted records whether the source text was quoted. Matters
	// for downstream rules that treat quoted identifiers differently
	// (e.g. "id" vs id when a reserved word collides). Note: both
	// equality and map-key behavior fold through this flag, so two
	// Identifiers with the same name but different wasQuoted DO
	// compare unequal — which matches Java's Identifier.equals.
	wasQuoted bool
}

// New constructs an Identifier by normalizing raw per SQL rules.
// When caseSensitive is true, unquoted identifiers retain their
// source casing; otherwise they're upper-cased. Quoted identifiers
// always retain case regardless of caseSensitive.
func New(raw string, caseSensitive bool) Identifier {
	if raw == "" {
		return Identifier{}
	}
	if isQuoted(raw, '"') || isQuoted(raw, '\'') {
		return Identifier{name: raw[1 : len(raw)-1], wasQuoted: true}
	}
	if caseSensitive {
		return Identifier{name: raw}
	}
	return Identifier{name: strings.ToUpper(raw)}
}

// NewUnquoted is the common path — a case-insensitive bare
// identifier. Equivalent to New(raw, false).
func NewUnquoted(raw string) Identifier {
	return New(raw, false)
}

// Name returns the normalized identifier text. Two Identifiers
// with the same Name AND same WasQuoted are equal via `==`.
func (i Identifier) Name() string { return i.name }

// WasQuoted reports whether the source text was quoted. Callers
// that need to preserve user intent (e.g. "keyword" vs keyword)
// check this flag.
func (i Identifier) WasQuoted() bool { return i.wasQuoted }

// IsZero reports whether i is the zero-value Identifier (empty).
// Useful for nil-check replacement since Identifier is a value type.
func (i Identifier) IsZero() bool { return i.name == "" }

// String implements fmt.Stringer.
func (i Identifier) String() string { return i.name }

// EqualsIgnoreQuoting compares by Name only, ignoring the quoting
// flag. For most lookups a quoted-vs-unquoted distinction doesn't
// matter — e.g. an `ORDER BY` clause referring to `"age"` still
// targets the same column as a SELECT projecting `age`. Use `==` on
// Identifier when the quoting distinction matters (resolving
// against a reserved-word shadow).
func (i Identifier) EqualsIgnoreQuoting(other Identifier) bool {
	return i.name == other.name
}

// NormalizeString is the lower-level helper underlying New. Exposed
// for call sites that have a raw string and don't need the full
// Identifier wrapper (e.g. dedup keys in the parser). Mirrors Java's
// `SemanticAnalyzer.normalizeString`.
//
//   - Empty string → empty string.
//   - Quoted (single or double) → strip quotes verbatim.
//   - Unquoted + caseSensitive → return unchanged.
//   - Unquoted + !caseSensitive → upper-case.
func NormalizeString(s string, caseSensitive bool) string {
	if s == "" {
		return ""
	}
	if isQuoted(s, '"') || isQuoted(s, '\'') {
		return s[1 : len(s)-1]
	}
	if caseSensitive {
		return s
	}
	return strings.ToUpper(s)
}

// isQuoted reports whether s starts and ends with quoteRune and has
// at least one character between them. `""` and `”` (empty quoted
// strings) do NOT count — otherwise New("”", false) would produce
// an Identifier{name: "", wasQuoted: true} that's IsZero() == true
// but != Identifier{} under value equality. Reject the empty-quoted
// case up front.
func isQuoted(s string, quoteRune byte) bool {
	if len(s) < 3 {
		return false
	}
	return s[0] == quoteRune && s[len(s)-1] == quoteRune
}
