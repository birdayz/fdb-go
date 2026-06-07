package values

import "strings"

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
//   - exactly 1 character → escape-aware transformation
//     (escape+`_` → literal `_`, escape+`%` → literal `%`).
//   - other length → returns nil (Java throws SemanticException;
//     Go's seed defers to evaluator-side reporting). Documented as
//     a planner-checked precondition.
//
// Java's `LikeOperatorValue.likeOperation` calls
// `Pattern.compile(rhs)` WITHOUT DOTALL — Go's default regexp
// behavior (`.` does NOT match `\n`) is already aligned.
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
func (v *PatternForLikeValue) Evaluate(evalCtx any) any {
	res, err := v.EvaluateErr(evalCtx)
	if err != nil {
		panic(err)
	}
	return res
}

// EvaluateErr is the error-returning twin of Evaluate (RFC-091).
func (v *PatternForLikeValue) EvaluateErr(evalCtx any) (any, error) {
	if v.PatternChild == nil {
		return nil, nil
	}
	patVal, err := v.PatternChild.EvaluateErr(evalCtx)
	if err != nil {
		return nil, err
	}
	pat, ok := patVal.(string)
	if !ok {
		return nil, nil
	}
	var esc string
	hasEscape := false
	if v.EscapeChild != nil {
		raw, err := v.EscapeChild.EvaluateErr(evalCtx)
		if err != nil {
			return nil, err
		}
		if raw != nil {
			s, ok := raw.(string)
			if !ok || len(s) != 1 {
				// Java throws SemanticException.ESCAPE_CHAR_OF_LIKE_OPERATOR_IS_NOT_SINGLE_CHAR;
				// the seed surfaces this as nil to the eval contract.
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
		// Escape-aware: <esc>_ → _, <esc>% → %.
		if hasEscape && i+1 < len(pat) && string(c) == esc {
			next := pat[i+1]
			if next == '_' || next == '%' {
				b.WriteByte(next)
				i++
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
