package values

import (
	"strings"
	"unicode/utf8"
)

// isSingleUTF16Unit reports whether s is exactly one UTF-16 code
// unit long — Java's `String.length() == 1`. That is one rune, and
// that rune in the BMP (<= U+FFFF); an astral rune encodes as a
// surrogate pair and has Java length 2.
func isSingleUTF16Unit(s string) bool {
	r, size := utf8.DecodeRuneInString(s)
	if size == 0 || size != len(s) {
		return false // empty, or more than one rune
	}
	return r <= 0xFFFF // BMP rune = 1 UTF-16 unit; astral = 2
}

// PatternForLikeValue is the SQL `patternForLike(pattern, escape)`
// function — converts a SQL LIKE pattern (with `%` / `_`
// wildcards and an optional escape char) to a regex-form string,
// wrapped in `^...$`. Mirrors Java's
// `com.apple.foundationdb.record.query.plan.cascades.values.PatternForLikeValue`.
//
// This Value is part of Java's LIKE-operator surface: Java's
// `LikeOperatorValue.eval` consumes the regex string produced here
// via `java.util.regex.Pattern`. Our Go LikeOperatorValue does NOT
// consume the regex — it routes through the canonical
// `values.LikeMatch` matcher, which works DIRECTLY on the SQL
// pattern with `%` / `_` (no regex involvement). PatternForLikeValue
// is therefore a planner-side surface only in Go: SQL queries that
// reference `patternForLike(...)` lower to this Value, but the
// produced regex string isn't consumed by any Go runtime path.
//
// We still port it because:
//   - It's a SQL-callable function that may appear in user queries
//     (Java's grammar exposes `patternForLike` as a builtin).
//   - Plan-level equivalence with Java requires the same Value tree
//     shape — even when the actual eval path differs.
//   - Direct Java → Go SQL plan ports won't fail with "unknown
//     function" when this surface is reached.
//
// Result type: NotNullString (the regex form is always a string).
//
// Eval contract (matches Java):
//   - patternChild evaluates to a string. If NULL, eval returns NULL.
//   - escapeChild evaluates to a string OR NULL.
//   - NULL → standard transformation (no escape).
//   - exactly 1 UTF-16 code unit → escape-aware transformation
//     (escape+`_` → literal `_`, escape+`%` → literal `%`). Java
//     checks `escapeChar.length() == 1` (PatternForLikeValue.java:109),
//     which counts UTF-16 units: any single BMP rune (<= U+FFFF)
//     passes, an astral rune is TWO units and fails.
//   - other length → returns nil (Java throws SemanticException;
//     Go defers to evaluator-side reporting). Documented as
//     a planner-checked precondition.
//
// The produced regex is JAVA-regex-shaped, for Java's evaluation
// semantics: `LikeOperatorValue.likeOperation`
// (LikeOperatorValue.java:93-99) compiles it with NO flags and runs
// `.find()`, so its `.` rejects Java's five line-terminator code
// points (`\n`, `\r`, U+0085, U+2028, U+2029 — MORE than Go regexp's
// default, which only excludes `\n`) and its default-mode `$`
// tolerates one final line terminator. Do NOT feed this string to Go
// `regexp` and expect Java's answer; `values.LikeMatch` implements
// the composed Java semantics directly, and
// TestLikeMatch_CrossCheckSQLPatternToRegex proves the two agree.
type PatternForLikeValue struct {
	PatternChild Value
	EscapeChild  Value
}

// NewPatternForLikeValue constructs the value with required pattern
// and optional escape children.
func NewPatternForLikeValue(pattern, escape Value) *PatternForLikeValue {
	return &PatternForLikeValue{PatternChild: pattern, EscapeChild: escape}
}

// Children returns [pattern, escape].
func (v *PatternForLikeValue) Children() []Value {
	return []Value{v.PatternChild, v.EscapeChild}
}

// Name returns the SQL function name.
func (*PatternForLikeValue) Name() string { return "patternForLike" }

// Type returns NotNullString.
func (*PatternForLikeValue) Type() Type { return NotNullString }

// Evaluate produces the regex-form string with `^...$` anchors.
// Returns nil if the pattern is NULL or the escape is malformed.
func (v *PatternForLikeValue) Evaluate(evalCtx any) (any, error) {
	if v.PatternChild == nil {
		return nil, nil
	}
	patRaw, err := v.PatternChild.Evaluate(evalCtx)
	if err != nil {
		return nil, err
	}
	pat, ok := patRaw.(string)
	if !ok {
		return nil, nil
	}
	var esc string
	hasEscape := false
	if v.EscapeChild != nil {
		raw, err := v.EscapeChild.Evaluate(evalCtx)
		if err != nil {
			return nil, err
		}
		if raw != nil {
			s, ok := raw.(string)
			// Java's precondition is `escapeChar.length() == 1` in
			// UTF-16 code units (PatternForLikeValue.java:109): one
			// BMP rune (<= U+FFFF, e.g. 'é') passes, while an astral
			// rune ('😀') is a surrogate PAIR — two units — and fails
			// exactly like a two-character string. A byte-length
			// check would wrongly reject every multi-byte BMP rune.
			if !ok || !isSingleUTF16Unit(s) {
				// Java throws SemanticException.ESCAPE_CHAR_OF_LIKE_OPERATOR_IS_NOT_SINGLE_CHAR;
				// Go surfaces this as nil to the eval contract.
				return nil, nil
			}
			esc = s
			hasEscape = true
		}
	}
	return "^" + sqlPatternToRegex(pat, esc, hasEscape) + "$", nil
}

// sqlPatternToRegex converts a SQL LIKE pattern to a regex pattern.
// Mirrors Java's REPLACE_MAP table: `%` → `.*`, `_` → `.`, regex
// metacharacters get escaped. With an explicit escape character,
// `<esc>_` and `<esc>%` map to literal `_` and `%`.
//
// The transformation is a left-to-right walk so the escape rule
// fires before the wildcard rule on the same character (matches
// Java's StringUtils.replaceEach pass-order semantics — first-match-
// wins per longest-key-first).
func sqlPatternToRegex(pat, esc string, hasEscape bool) string {
	var b strings.Builder
	b.Grow(len(pat) + 8)
	for i := 0; i < len(pat); i++ {
		c := pat[i]
		// Escape-aware: <esc>_ → _, <esc>% → %. Prefix-match the
		// whole escape string: a multi-byte BMP escape rune ('é') is
		// a legal single-UTF-16-unit escape in Java and spans
		// several bytes here.
		if hasEscape && strings.HasPrefix(pat[i:], esc) && i+len(esc) < len(pat) {
			next := pat[i+len(esc)]
			if next == '_' || next == '%' {
				b.WriteByte(next)
				i += len(esc)
				continue
			}
			// Standalone <esc> char: fall through to per-char rules.
		}
		switch c {
		case '%':
			b.WriteString(".*")
		case '_':
			b.WriteByte('.')
		case '|', '.', '^', '$', '\\', '*', '+', '?', '[', ']', '{', '}', '(', ')':
			b.WriteByte('\\')
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}
