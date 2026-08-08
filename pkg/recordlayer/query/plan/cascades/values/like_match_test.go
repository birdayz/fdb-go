package values

import (
	"regexp"
	"strings"
	"testing"
)

func TestLikeMatch(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		pattern string
		s       string
		escape  rune
		want    bool
	}{
		// Basic exact / mismatch.
		{"exact match", "hello", "hello", 0, true},
		{"no match", "world", "hello", 0, false},

		// % wildcard.
		{"% at end", "hel%", "hello", 0, true},
		{"% at start", "%llo", "hello", 0, true},
		{"% both ends", "%ell%", "hello", 0, true},
		{"multiple %", "%b%e%", "abcdef", 0, true},
		{"% matching empty substring", "a%c", "ac", 0, true},
		{"all %", "%%%", "anything", 0, true},
		{"only %", "%", "", 0, true},

		// _ wildcard.
		{"_ single char", "a_c", "abc", 0, true},
		{"_ wrong length", "a_c", "abcd", 0, false},
		{"single _", "_", "a", 0, true},

		// Mixed wildcards.
		{"mixed % and _", "a_c%f", "abcdef", 0, true},

		// Empty string / pattern.
		{"empty string empty pattern", "", "", 0, true},
		{"empty string % pattern", "%", "", 0, true},
		{"empty string _ pattern", "_", "", 0, false},

		// Pattern longer than string.
		{"pattern longer", "ab", "a", 0, false},

		// Escape character.
		{"escape literal %", `a\%b`, "a%b", '\\', true},
		{"escape literal _", `a\_b`, "a_b", '\\', true},
		{"escape % no match on wildcard", `a\%b`, "aXb", '\\', false},
		// A dangling escape opens no escape sequence (Java installs
		// only `<esc>_` and `<esc>%`), so it is an ordinary literal
		// and still demands a character.
		{"dangling escape needs the escape rune in the input", `ab\`, "ab", '\\', false},
		{"dangling escape matches the escape rune", `ab\`, `ab\`, '\\', true},
		// No escaped-escape entry exists, so `\\` is two literals.
		{"no escaped-escape", `a\\b`, `a\\b`, '\\', true},
		{"_ with escape=0", "a_c", "abc", 0, true},

		// Unicode.
		{"unicode _ match", "caf_", "café", 0, true},
		{"unicode % match", "日%語", "日本語", 0, true},
		{"unicode exact", "日本語", "日本語", 0, true},
		{"unicode _ no match", "caf_", "cafe!", 0, false},

		// Backtracking stress.
		{"worst-case backtrack", "%a%a%a%a", "aaaa", 0, true},
		{"backtrack miss", "%a%a%a%a", "aaab", 0, false},

		// Miscellaneous edge cases.
		{"% does not match across nothing with trailing literal", "a%b", "a", 0, false},
		{"multiple _ exact width", "___", "abc", 0, true},
		{"multiple _ too short", "___", "ab", 0, false},
		{"multiple _ too long", "___", "abcd", 0, false},
		{"pattern only _", "____", "test", 0, true},
		{"literal after %", "%xyz", "abcxyz", 0, true},
		{"literal after % no match", "%xyz", "abcxy", 0, false},
		{"escape in middle with wildcard after", `a\%_%`, "a%bc", '\\', true},
		{"escape zero passed explicitly", "a%c", "abc", 0, true},
		{"empty pattern non-empty string", "", "a", 0, false},
		{"trailing % matches remainder", "a%", "abcdef", 0, true},
		{"trailing % with empty remainder", "a%", "a", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := LikeMatch(tc.pattern, tc.s, tc.escape)
			if got != tc.want {
				t.Fatalf("LikeMatch(%q, %q, %q) = %v, want %v",
					tc.pattern, tc.s, tc.escape, got, tc.want)
			}
		})
	}
}

// TestLikeMatch_JavaNewlineSemantics pins Java's default-mode regex
// behaviour on line terminators, measured against a real JDK
// (Pattern.compile with no flags + find(), LikeOperatorValue.java:93-99
// over PatternForLikeValue.java:116's `^...$` shape). Two independent
// directions, both pinned:
//
//  1. No DOTALL — `_`/`%` reject the five Java line terminators.
//  2. Default `$` tolerates exactly ONE final line terminator
//     ("\r\n" as a unit; `$` never matches between its `\r` and `\n`).
//
// A fix satisfying only one direction fails the other's rows here.
func TestLikeMatch_JavaNewlineSemantics(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		pattern string
		s       string
		escape  rune
		want    bool
	}{
		// MEASURED against a real JDK.
		{"_ rejects \\n", "a_b", "a\nb", 0, false},
		{"% rejects \\n", "a%b", "a\nb", 0, false},
		{"lone \\n vs _", "_", "\n", 0, false},
		{"$ before final \\n", "abc", "abc\n", 0, true},
		{"$ before final LS", "a", "a\u2028", 0, true},
		// The sentinel that keeps a half-fix honest: `.*` matches
		// empty and `$` sits before the trailing terminator, so a
		// naive (?s)-drop (RE2 `$` = end-of-text) breaks it.
		{"lone \\n vs %", "%", "\n", 0, true},

		// Derived from java.util.regex.Pattern$Dollar semantics.
		{"$ before final \\r\\n unit", "a", "a\r\n", 0, true},
		{"$ never between \\r and \\n", "a\r", "a\r\n", 0, false},
		{"$ before final \\r", "a", "a\r", 0, true},
		{"$ before final NEL", "a", "a\u0085", 0, true},
		{"$ before final PS", "a", "a\u2029", 0, true},
		{"only ONE final terminator", "a", "a\n\n", 0, false},
		{"% then two terminators", "%", "\n\n", 0, false},
		{"% then final \\r\\n unit", "%", "\r\n", 0, true},
		{"literal \\n in pattern matches", "a\nb", "a\nb", 0, true},
		{"literal \\n after %", "%\n", "ab\n", 0, true},
		{"% cannot reach past \\n to end", "a%", "a\nb", 0, false},
		{"_ rejects \\r mid-string", "a_b", "a\rb", 0, false},
		{"% rejects NEL mid-string", "a%b", "a\u0085b", 0, false},
		{"lone LS vs _", "_", "\u2028", 0, false},

		// Escape path composes with `$` tolerance.
		{"escaped % then final terminator", `a\%`, "a%\n", '\\', true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := LikeMatch(tc.pattern, tc.s, tc.escape)
			if got != tc.want {
				t.Fatalf("LikeMatch(%q, %q, %q) = %v, want %v",
					tc.pattern, tc.s, tc.escape, got, tc.want)
			}
		})
	}
}

// TestLikeMatch_CrossCheckSQLPatternToRegex binds the two in-package
// expressions of the LIKE spec to each other: values.LikeMatch (the
// direct matcher) and values.sqlPatternToRegex (Java's regex
// translation, PatternForLikeValue.java:62-116), evaluated under
// Java's default-mode `.`/`$` semantics, over an exhaustive ASCII
// pattern/subject/escape grid that includes wildcards, the escape
// rune, regex metacharacters and line terminators. Any disagreement
// is a conformance bug in one of the two.
func TestLikeMatch_CrossCheckSQLPatternToRegex(t *testing.T) {
	t.Parallel()
	patAlpha := []string{"a", "b", "%", "_", `\`, ".", "\n", "\r"}
	subjAlpha := []string{"a", "b", `\`, ".", "\n", "\r", "\u0085"}
	escapes := []struct {
		esc string
		r   rune
	}{
		{"", 0},
		{`\`, '\\'},
		{"%", '%'}, // escape colliding with a wildcard
	}
	patterns := enumerateStrings(patAlpha, 3)
	subjects := enumerateStrings(subjAlpha, 3)
	for _, e := range escapes {
		hasEscape := e.esc != ""
		for _, pat := range patterns {
			body := sqlPatternToRegex(pat, e.esc, hasEscape)
			re, err := compileJavaShapedRegex(body)
			if err != nil {
				t.Fatalf("pattern %q (esc %q) produced uncompilable regex %q: %v", pat, e.esc, body, err)
			}
			for _, s := range subjects {
				got := LikeMatch(pat, s, e.r)
				want := javaFindAnchored(re, s)
				if got != want {
					t.Fatalf("divergence: pattern=%q s=%q escape=%q: LikeMatch=%v, sqlPatternToRegex(%q) under Java semantics=%v",
						pat, s, e.esc, got, body, want)
				}
			}
		}
	}
}

// TestLikeMatch_EscapeFallthroughFixesTheConstantPrefix pins, on the live
// matcher, the four ESCAPE fallthrough shapes that decide where a pattern's
// CONSTANT LITERAL PREFIX ends — the longest initial literal run before the
// first UNESCAPED `%` or `_`.
//
// Nothing derives such a prefix today. RFC-216 §4 designs the extractor that
// would (`values.LikeConstantPrefix`) and tabulates the required prefix per
// ESCAPE hazard. That table is a claim about THIS matcher's semantics, and a
// table in a design document cannot fail — these assertions are what make it
// checkable, and what goes red if the matcher's ESCAPE handling moves under it.
// If that happens, the RFC's table is what needs re-deriving, not this test.
//
// Each case is a PAIR: a subject inside the claimed prefix's byte range that the
// pattern MATCHES, and one it REJECTS. One direction alone cannot locate a
// boundary — only the rejection rules out the boundary being somewhere else.
//
// The `escape == '%'` pair is the sharpest, and was the unpinned one. Java's
// replacement table has exactly two escape entries, `<esc>_` and `<esc>%`, so an
// escape rune not followed by `_`/`%` falls through to the ORDINARY rules — which
// for `%` means falling through to the METACHARACTER rules. The same rune is
// therefore a literal in `'%%abc'` and a wildcard in `'%abc'`, giving prefixes
// `%abc` and empty. TestLikeMatch's ESCAPE rows cover the DANGLING `%` direction
// (`"a%"` with escape `%` still wildcards); the before-an-ordinary-character
// direction is pinned here.
func TestLikeMatch_EscapeFallthroughFixesTheConstantPrefix(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		pattern string
		escape  rune
		// prefix is the constant literal prefix RFC-216 §4 requires an
		// extractor to derive from pattern. Nothing computes it — no
		// extractor exists — it is the value the two subjects witness.
		prefix  string
		matches string
		rejects string
		why     string
	}{
		{
			name:    "escape_rune_is_percent_escaped_pair_is_literal",
			pattern: "%%abc", escape: '%', prefix: "%abc",
			matches: "%abc", rejects: "abc",
			why: "`%%` is the `<esc>%` entry, so a LITERAL `%` opens the prefix. If the " +
				"two subjects swap verdicts, `%%` is behaving as a wildcard and the " +
				"derived prefix is empty rather than \"%abc\".",
		},
		{
			name:    "escape_rune_is_percent_before_ordinary_char_still_wildcards",
			pattern: "%abc", escape: '%', prefix: "",
			matches: "xabc", rejects: "xabd",
			why: "`%` before an ordinary character matches no escape entry and falls " +
				"through to the metacharacter rules, so it is the WILDCARD and the " +
				"constant prefix is EMPTY. A rewrite must bail out here: deriving " +
				"\"%abc\" would emit a range that excludes the matched subject, which " +
				"the predicate accepts — dropped rows, the LOSING direction.",
		},
		{
			name:    "no_escaped_escape_both_runes_stay_literal",
			pattern: "a!!b%", escape: '!', prefix: "a!!b",
			matches: "a!!bZZ", rejects: "ab",
			why: "Java installs no `<esc><esc>` entry, so NEITHER `!` is consumed as an " +
				"escape and both are literals: the prefix is \"a!!b\", four runes, not " +
				"the three an escape-collapsing scanner would produce.",
		},
		{
			name:    "dangling_escape_is_a_literal_in_the_prefix",
			pattern: "abc!", escape: '!', prefix: "abc!",
			matches: "abc!", rejects: "abc",
			why: "A dangling escape is neither malformed nor a no-match: it is a literal, " +
				"so it belongs IN the prefix. Truncating to \"abc\" would emit a range " +
				"containing the rejected subject — which is the superset direction the " +
				"mandatory residual covers (RFC-216 §2).",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if !strings.HasPrefix(c.matches, c.prefix) {
				t.Fatalf("setup is wrong: matched subject %q is not in the byte-prefix "+
					"range of %q", c.matches, c.prefix)
			}
			if !LikeMatch(c.pattern, c.matches, c.escape) {
				t.Fatalf("LikeMatch(%q, %q, %q) = false, want true\n  prefix under test: %q\n  %s",
					c.pattern, c.matches, c.escape, c.prefix, c.why)
			}
			if LikeMatch(c.pattern, c.rejects, c.escape) {
				t.Fatalf("LikeMatch(%q, %q, %q) = true, want false\n  prefix under test: %q\n  %s",
					c.pattern, c.rejects, c.escape, c.prefix, c.why)
			}
		})
	}
}

// enumerateStrings returns every concatenation of alphabet elements
// of length 0..maxLen.
func enumerateStrings(alphabet []string, maxLen int) []string {
	out := []string{""}
	prev := []string{""}
	for l := 1; l <= maxLen; l++ {
		next := make([]string, 0, len(prev)*len(alphabet))
		for _, p := range prev {
			for _, a := range alphabet {
				next = append(next, p+a)
			}
		}
		out = append(out, next...)
		prev = next
	}
	return out
}

// compileJavaShapedRegex converts the Java-regex body produced by
// sqlPatternToRegex to an RE2 pattern with Java default-mode `.`
// semantics (rejecting all five Java line terminators, not just
// RE2's `\n`) and anchors it hard. `$` tolerance is applied by
// javaFindAnchored, not here.
func compileJavaShapedRegex(body string) (*regexp.Regexp, error) {
	const javaDot = `[^\n\r\x{0085}\x{2028}\x{2029}]`
	var b strings.Builder
	b.WriteString(`\A(?:`)
	for i := 0; i < len(body); i++ {
		c := body[i]
		if c == '\\' && i+1 < len(body) {
			b.WriteByte(c)
			b.WriteByte(body[i+1])
			i++
			continue
		}
		if c == '.' {
			b.WriteString(javaDot)
			continue
		}
		b.WriteByte(c)
	}
	b.WriteString(`)\z`)
	return regexp.Compile(b.String())
}

// javaFindAnchored applies Java's `.find()` over a `^...$` pattern:
// full match, or full match with exactly one final line terminator
// stripped ("\r\n" as a unit — default `$` never matches between a
// final `\r` and `\n`). Deliberately re-derived here instead of
// calling the matcher's trimFinalLineTerminator so the cross-check
// does not restate the implementation.
func javaFindAnchored(re *regexp.Regexp, s string) bool {
	if re.MatchString(s) {
		return true
	}
	if strings.HasSuffix(s, "\r\n") {
		return re.MatchString(s[:len(s)-2])
	}
	for _, term := range []string{"\n", "\r", "\u0085", "\u2028", "\u2029"} {
		if strings.HasSuffix(s, term) {
			return re.MatchString(s[:len(s)-len(term)])
		}
	}
	return false
}

// Benchmarks use b.Loop() (Go 1.24+).

func BenchmarkLikeMatch_ExactMatch(b *testing.B) {
	for b.Loop() {
		LikeMatch("hello world", "hello world", 0)
	}
}

func BenchmarkLikeMatch_PercentWildcard(b *testing.B) {
	for b.Loop() {
		LikeMatch("%world", "hello world", 0)
	}
}

func BenchmarkLikeMatch_ComplexBacktrack(b *testing.B) {
	// Long input with a pattern that forces repeated backtracking.
	s := strings.Repeat("a", 200) + "b" + strings.Repeat("a", 200) + "c" + strings.Repeat("a", 200) + "d"
	pattern := "%a%b%c%d"
	for b.Loop() {
		LikeMatch(pattern, s, 0)
	}
}

func BenchmarkLikeMatch_WorstCaseNoMatch(b *testing.B) {
	// Pattern that forces O(n*m) backtracking without ever matching.
	s := strings.Repeat("a", 500)
	pattern := "%a%a%a%b"
	for b.Loop() {
		LikeMatch(pattern, s, 0)
	}
}

func BenchmarkLikeMatch_EscapeChar(b *testing.B) {
	for b.Loop() {
		LikeMatch(`a\%b\_c\\d`, `a%b_c\d`, '\\')
	}
}
