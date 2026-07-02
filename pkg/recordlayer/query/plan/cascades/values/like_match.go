package values

// LikeMatch implements SQL `LIKE` matching:
//   - `%` matches zero or more characters
//   - `_` matches exactly one character
//   - `escape` (if non-zero) makes the next character a literal
//
// Greedy backtrack; O(|pattern| * |s|) worst case. Returns true iff
// the pattern matches the whole string (SQL LIKE is anchored on
// both ends).
//
// Conformance contract: this is the canonical SQL LIKE matcher used
// by both the QueryPredicate-layer ComparisonLike (via predicates
// package) AND the Value-layer LikeOperatorValue. Java's
// `Comparisons.likeMatcher` is the spec; this implementation has
// been fuzz-tested against a regex oracle in
// `pkg/recordlayer/query/plan/cascades/predicates/comparisons_test.go`
// (FuzzLikeMatch / FuzzLikeMatchEscape). Any divergence between
// this and Java's `likeMatcher` is a conformance bug.
//
// Trailing-escape behaviour: a trailing escape rune (no following
// character) is MALFORMED → no match (a fuzz-found bug fix).
func LikeMatch(pattern, s string, escape rune) bool {
	p := []rune(pattern)
	str := []rune(s)
	pi, si := 0, 0
	starPi, starSi := -1, 0
	for si < len(str) {
		if pi < len(p) {
			if escape != 0 && p[pi] == escape {
				// Trailing escape (no following char) is malformed —
				// no match per the documented contract. Otherwise the
				// rune at pi+1 must match the input literally.
				if pi+1 >= len(p) {
					return false
				}
				if p[pi+1] == str[si] {
					pi += 2
					si++
					continue
				}
			} else {
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
	// Trailing wildcards still match. With escape, anything other than
	// `%` in the trailing position is a literal that requires
	// unconsumed input — no match.
	for pi < len(p) {
		if escape != 0 && p[pi] == escape {
			return false // trailing escape (or escape-sequence requiring more input)
		}
		if p[pi] != '%' {
			return false
		}
		pi++
	}
	return pi == len(p)
}
