package values

import "strings"

// LikeMatch implements SQL `LIKE` matching:
//   - `%` matches zero or more characters, none of which may be a
//     line terminator
//   - `_` matches exactly one character that is not a line terminator
//   - `escape` (if non-zero) makes a following `%` or `_` literal
//
// Greedy backtrack; O(|pattern| * |s|) worst case.
//
// Conformance contract: this is the ONE runtime SQL LIKE matcher. It
// backs the QueryPredicate-layer ComparisonLike (via the predicates
// package), the Value-layer LikeOperatorValue, and the map-backed
// INFORMATION_SCHEMA WHERE evaluator. Java's
// `PatternForLikeValue.eval` (PatternForLikeValue.java:96-117) +
// `LikeOperatorValue.likeOperation` (LikeOperatorValue.java:93-99) is
// the spec: Java rewrites the SQL pattern into `^<regex>$`, compiles
// it with `Pattern.compile` and NO flags, and matches via `.find()`.
// This matcher must return what that composition would.
//
// NEWLINE semantics — the observable consequences of "no flags":
//
//   - No DOTALL: `_` -> `.` and `%` -> `.*` do NOT match a line
//     terminator. Java's line-terminator code points in default mode
//     (java.util.regex.Pattern, "Line terminators") are `\n`, `\r`,
//     U+0085 (NEL), U+2028 (LS) and U+2029 (PS). A terminator in the
//     input can only be matched by a literal terminator in the
//     pattern. So `'a\nb' LIKE 'a_b'` and `'a\nb' LIKE 'a%b'` are
//     FALSE.
//   - No MULTILINE, so `^` only matches at index 0 — `.find()` on an
//     `^...$` pattern degenerates to an anchored match.
//   - Default `$` matches at end of input OR when the remaining
//     input is exactly one FINAL line terminator: "\n", "\r\n",
//     "\r", U+0085, U+2028 or U+2029 — and `$` never matches BETWEEN
//     the `\r` and `\n` of a final "\r\n"
//     (java.util.regex.Pattern$Dollar). So `'abc\n' LIKE 'abc'` is
//     TRUE, `'a' + U+2028 LIKE 'a'` is TRUE, and — because `.*` can
//     match empty with `$` sitting before the trailing terminator —
//     `'\n' LIKE '%'` is TRUE even though `%` cannot consume the
//     `\n` itself.
//
// ESCAPE semantics — Java's, exactly, and narrower than the SQL
// standard's. `PatternForLikeValue` builds its replacement table as
// exactly TWO escape entries on top of the metacharacter table:
//
//	.put(escapeChar + "_", "_")
//	.put(escapeChar + "%", "%")
//	.putAll(REPLACE_MAP)
//
// so an escape rune is consumed as an escape ONLY when `_` or `%`
// follows it. In every other position — before an ordinary
// character, before a second escape rune, or dangling at the end of
// the pattern — no escape entry can match, and the rune is instead
// rewritten by the ordinary per-character rules. Three consequences,
// each of which differs from the more common "escape makes the next
// character literal" reading, and each pinned by a test:
//
//   - A DANGLING escape is not malformed and is not a no-match: it
//     is the escape rune taken literally (or, if the escape rune is
//     itself `%` or `_`, taken as that wildcard). Java's own corpus
//     records this — `like.yamsql:92` runs
//     `B2 NOT LIKE 'Z' ESCAPE 'Z'` and excludes the two `'Z'` rows,
//     i.e. `'Z' LIKE 'Z' ESCAPE 'Z'` is TRUE. Java's comment there
//     concedes the SQL standard would raise 22025 instead, but the
//     pinned behaviour is the literal match.
//   - Escape before an ORDINARY character does not make that
//     character literal — the escape rune itself is the literal and
//     the next character is then read normally. `a\b` ESCAPE `\`
//     matches `a\b`, not `ab`.
//   - There is no escaped-escape: `a\\b` ESCAPE `\` is two literal
//     backslashes, not one.
//
// The escape rune falling through to the ordinary rules is what
// makes an escape rune of `%` or `_` still act as a wildcard in
// those positions, which is why the fallthrough is a fallthrough and
// not an unconditional literal.
//
// `values.sqlPatternToRegex` (PatternForLikeValue) is the same spec
// expressed as Java's regex translation. The two are cross-checked
// against each other by TestLikeMatch_CrossCheckSQLPatternToRegex in
// this package (an exhaustive ASCII pattern/subject/escape grid,
// evaluating the produced regex under Java's default-mode `.` and
// `$` semantics), and LikeMatch is independently fuzzed against a
// Java-semantics regex oracle by FuzzLikeMatch / FuzzLikeMatchEscape
// in `pkg/recordlayer/query/plan/cascades/predicates/comparisons_test.go`.
// Any divergence between them, or between either and Java, is a
// conformance bug.
func LikeMatch(pattern, s string, escape rune) bool {
	if likeMatchWhole(pattern, s, escape) {
		return true
	}
	// Java's default-mode `$` also matches just before a single FINAL
	// line terminator, so the regex accepts the input with exactly one
	// trailing terminator stripped. "\r\n" strips as a unit because
	// `$` never matches between its `\r` and `\n`.
	if trimmed, ok := trimFinalLineTerminator(s); ok {
		return likeMatchWhole(pattern, trimmed, escape)
	}
	return false
}

// likeMatchWhole matches the pattern against ALL of s (both ends
// anchored, no trailing-terminator tolerance — LikeMatch adds that).
func likeMatchWhole(pattern, s string, escape rune) bool {
	p := []rune(pattern)
	str := []rune(s)
	pi, si := 0, 0
	starPi, starSi := -1, 0
	for si < len(str) {
		if pi < len(p) {
			if escapedLiteralAt(p, pi, escape) {
				// `<esc>_` / `<esc>%` — the wildcard is a literal.
				if p[pi+1] == str[si] {
					pi += 2
					si++
					continue
				}
			} else {
				// Ordinary rules. An escape rune that did not open an
				// escape sequence lands here too, and is therefore a
				// wildcard when it happens to be `%` or `_` and a
				// literal otherwise — Java's REPLACE_MAP fallthrough.
				switch p[pi] {
				case '%':
					starPi = pi
					starSi = si
					pi++
					continue
				case '_':
					// `.` without DOTALL rejects line terminators.
					if !isJavaLineTerminator(str[si]) {
						pi++
						si++
						continue
					}
				default:
					if p[pi] == str[si] {
						pi++
						si++
						continue
					}
				}
			}
		}
		if starPi >= 0 && !isJavaLineTerminator(str[starSi]) {
			// Resume at the last `%`, letting it consume one more
			// rune. `.*` without DOTALL cannot consume a terminator,
			// and no earlier `%` could consume it either, so a
			// terminator at the resume point exhausts all backtracks.
			pi = starPi + 1
			starSi++
			si = starSi
			continue
		}
		return false
	}
	// Input exhausted. The pattern remainder matches only if every
	// remaining element consumes nothing — i.e. only `%` wildcards.
	// An escaped `_` / `%` is a literal and still needs a character.
	for pi < len(p) {
		if escapedLiteralAt(p, pi, escape) {
			return false
		}
		if p[pi] != '%' {
			return false
		}
		pi++
	}
	return true
}

// isJavaLineTerminator reports whether r is one of the five code
// points java.util.regex.Pattern treats as a line terminator in
// default (non-UNIX_LINES) mode: `\n`, `\r`, NEL, LS, PS. These are
// what `.` refuses to match without DOTALL and what default-mode `$`
// tolerates as a single final terminator.
func isJavaLineTerminator(r rune) bool {
	return r == '\n' || r == '\r' || r == '\u0085' || r == '\u2028' || r == '\u2029'
}

// trimFinalLineTerminator strips exactly one final Java line
// terminator from s: the two-rune sequence "\r\n" as a unit
// (default-mode `$` never matches between a final `\r` and `\n`),
// else one terminator rune. ok=false if s does not end in one.
func trimFinalLineTerminator(s string) (string, bool) {
	if strings.HasSuffix(s, "\r\n") {
		return s[:len(s)-2], true
	}
	for _, term := range []string{"\n", "\r", "\u0085", "\u2028", "\u2029"} {
		if strings.HasSuffix(s, term) {
			return s[:len(s)-len(term)], true
		}
	}
	return "", false
}

// escapedLiteralAt reports whether pattern position pi opens an
// escape sequence — the escape rune followed by `_` or `%`, the only
// two sequences Java's PatternForLikeValue recognises. Everywhere
// else the escape rune is an ordinary character.
func escapedLiteralAt(p []rune, pi int, escape rune) bool {
	if escape == 0 || p[pi] != escape || pi+1 >= len(p) {
		return false
	}
	return p[pi+1] == '_' || p[pi+1] == '%'
}
