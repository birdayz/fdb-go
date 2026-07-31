package values

// LikeMatch implements SQL `LIKE` matching:
//   - `%` matches zero or more characters
//   - `_` matches exactly one character
//   - `escape` (if non-zero) makes a following `%` or `_` literal
//
// Greedy backtrack; O(|pattern| * |s|) worst case. Returns true iff
// the pattern matches the whole string (SQL LIKE is anchored on
// both ends).
//
// Conformance contract: this is the ONE SQL LIKE matcher. It backs
// the QueryPredicate-layer ComparisonLike (via the predicates
// package), the Value-layer LikeOperatorValue, and the map-backed
// INFORMATION_SCHEMA WHERE evaluator. Java's
// `PatternForLikeValue.eval` + `LikeOperatorValue.likeOperation` is
// the spec: Java translates the SQL pattern to a regex and matches
// it anchored, and this matcher must return what that regex would.
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
// expressed as Java's regex translation; the two are cross-checked
// against each other by FuzzLikeMatch / FuzzLikeMatchEscape in
// `pkg/recordlayer/query/plan/cascades/predicates/comparisons_test.go`.
// Any divergence between them, or between either and Java, is a
// conformance bug.
func LikeMatch(pattern, s string, escape rune) bool {
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
					pi++
					si++
					continue
				default:
					if p[pi] == str[si] {
						pi++
						si++
						continue
					}
				}
			}
		}
		if starPi >= 0 {
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
