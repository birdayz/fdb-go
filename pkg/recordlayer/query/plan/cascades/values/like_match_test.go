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
		// Escape before an ordinary character opens no escape entry:
		// the escape rune itself is the literal and the next character
		// is read normally, so `a\b` demands the backslash.
		{"escape before an ordinary char is itself the literal", `a\b`, `a\b`, '\\', true},
		{"escape before an ordinary char does not make it literal", `a\b`, "ab", '\\', false},
		// The escape rune falling through to the ORDINARY rules means
		// falling through to the METACHARACTER rules too, so an escape
		// rune of `%` is still the wildcard where it opens no entry.
		{"dangling escape equal to % still wildcards", "a%", "aXY", '%', true},
		{"escaped % when the escape rune is itself %", "%%b", "%b", '%', true},
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
	// "A" is here so that literal comparison is checked for CASE
	// SENSITIVITY: without an uppercase subject rune, a matcher that
	// case-folded its literal comparison would agree with the regex on
	// every cell of this grid.
	subjAlpha := []string{"a", "A", "b", `\`, ".", "\n", "\r", "\u0085"}
	escapes := []struct {
		esc string
		r   rune
	}{
		{"", 0},
		{`\`, '\\'},
		{"%", '%'}, // escape colliding with the `%` wildcard
		{"_", '_'}, // escape colliding with the `_` wildcard
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

// TestLikeMatch_ConstantPrefixBoundary pins matcher behaviour a constant-prefix
// claim would rest on, for the seven patterns listed below and their own three
// subjects each. It does NOT establish a universal: it is not a statement about
// all pattern classes, and it cannot check any extractor's return value —
// nothing in the tree derives a prefix (there is no extractor and no rule that
// wants one; TODO.md CQ-33), so P is ASSERTED here rather than computed, and a
// future extractor returning "" for everything would leave this file green.
//
// What each case does prove, scoped to itself:
//
//	MAXIMALITY   the two matched subjects DIVERGE at byte offset len(P) — they
//	             differ there, or one ends exactly there. No longer prefix is
//	             common to THESE TWO, so a P too SHORT for this pattern fails
//	             here, P = "" included. This half is airtight for the fixture.
//	RESIDUAL     a subject INSIDE P's byte range that the pattern REJECTS. The
//	             byte range is therefore a STRICT superset of the predicate for
//	             this pattern, which is what forbids dropping the residual LIKE
//	             after emitting such a range.
//
// SOUNDNESS — that every match begins with P — is only SAMPLED here: the two
// matched subjects are checked, and two subjects cannot quantify over all
// matches, so a P that is too long survives unless one of them witnesses it.
// Mutating the matcher makes that concrete: folding case in the literal
// comparison makes LikeMatch("a_b%","AXB",0) true, so the asserted "a" is
// unsound while every assertion in this file still passes. Quantified soundness
// belongs to the exhaustive grid instead — which is why
// TestLikeMatch_CrossCheckSQLPatternToRegex carries an uppercase subject rune
// and an escape rune of `_`: those were its two blind spots, and matcher
// mutations of exactly that shape passed through both tests before they existed.
//
// The ESCAPE shapes are the ones a hand-rolled scanner gets wrong, because
// Java's replacement table has exactly TWO escape entries, `<esc>_` and
// `<esc>%`. An escape rune not followed by `_`/`%` falls through to the
// ORDINARY rules — which for `%` means falling through to the METACHARACTER
// rules, so the SAME rune is a literal in `'%%abc'` and a wildcard in `'%abc'`.
func TestLikeMatch_ConstantPrefixBoundary(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		pattern string
		escape  rune
		// prefix is the asserted constant literal prefix. Nothing
		// computes it; the three witnesses below locate it.
		prefix string
		// matchA and matchB both match, and must diverge at byte
		// offset len(prefix) — that is the maximality witness.
		matchA string
		matchB string
		// rejectInRange starts with prefix and does NOT match.
		rejectInRange string
		why           string
	}{
		{
			name:    "underscore_ends_the_prefix",
			pattern: "a_b%", escape: 0, prefix: "a",
			matchA: "aXb", matchB: "aYb", rejectInRange: "aXXb",
			why: "The scan stops at the first unescaped wildcard, so only \"a\" is " +
				"constant. `_` consumes exactly one rune and it is not pinned to any " +
				"value, which is what the two matched subjects show.",
		},
		{
			name:    "escaped_wildcard_stays_inside_the_prefix",
			pattern: "a!%b%", escape: '!', prefix: "a%b",
			matchA: "a%b", matchB: "a%bZ", rejectInRange: "a%b\nZ",
			why: "`!%` is the `<esc>%` entry, so the `%` is a LITERAL and belongs IN the " +
				"prefix. A scanner that stopped at the first `%` byte would derive \"a\" " +
				"— sound but three bytes shorter, i.e. a needlessly wide range. The " +
				"rejected subject is the strict-superset witness: the trailing `%` " +
				"is `.*` with no DOTALL, so it cannot cross the internal \\n.",
		},
		{
			name:    "escape_before_an_ordinary_char_is_itself_the_literal",
			pattern: "a!b%", escape: '!', prefix: "a!b",
			matchA: "a!b", matchB: "a!bZ", rejectInRange: "a!b\nZ",
			why: "`!` before an ordinary character opens no escape entry, so the `!` " +
				"itself is the literal and the `b` is read normally. A scanner using the " +
				"usual \"escape makes the next character literal\" reading derives \"ab\", " +
				"which no matched subject begins with — the dropped-rows direction.",
		},
		{
			name:    "escape_rune_is_percent_escaped_pair_is_literal",
			pattern: "%%abc", escape: '%', prefix: "%abc",
			matchA: "%abc", matchB: "%abc\n", rejectInRange: "%abcZ",
			why: "`%%` is the `<esc>%` entry, so a LITERAL `%` opens the prefix. If `%%` " +
				"were behaving as a wildcard the prefix would be empty, and matchA/matchB " +
				"would no longer be pinned to a common \"%abc\". matchB is the ONE final " +
				"line terminator Java's `$` tolerates, which is also why a wildcard-free " +
				"pattern is not an equality.",
		},
		{
			name:    "escape_rune_is_percent_before_ordinary_char_still_wildcards",
			pattern: "%abc", escape: '%', prefix: "",
			matchA: "xabc", matchB: "abc", rejectInRange: "xabd",
			why: "`%` before an ordinary character matches no escape entry and falls " +
				"through to the METACHARACTER rules, so it is the WILDCARD and the " +
				"constant prefix is EMPTY. Deriving \"%abc\" here would emit a range that " +
				"excludes matchA, which the predicate accepts — dropped rows.",
		},
		{
			name:    "no_escaped_escape_both_runes_stay_literal",
			pattern: "a!!b%", escape: '!', prefix: "a!!b",
			matchA: "a!!b", matchB: "a!!bZ", rejectInRange: "a!!b\nZ",
			why: "Java installs no `<esc><esc>` entry, so NEITHER `!` is consumed as an " +
				"escape and both are literals: the prefix is \"a!!b\", four runes, not the " +
				"three an escape-collapsing scanner would produce.",
		},
		{
			name:    "dangling_escape_is_a_literal_in_the_prefix",
			pattern: "abc!", escape: '!', prefix: "abc!",
			matchA: "abc!", matchB: "abc!\n", rejectInRange: "abc!Z",
			why: "A dangling escape is neither malformed nor a no-match: it is a literal, " +
				"so it belongs IN the prefix. Truncating to \"abc\" would leave matchA and " +
				"matchB agreeing at offset 3, i.e. it is not maximal.",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			// SOUNDNESS. Fails when prefix is too long.
			for _, m := range []string{c.matchA, c.matchB} {
				if !LikeMatch(c.pattern, m, c.escape) {
					t.Fatalf("LikeMatch(%q, %q, escape %q) = false, want true\n"+
						"  asserted prefix: %q\n  %s", c.pattern, m, c.escape, c.prefix, c.why)
				}
				if !strings.HasPrefix(m, c.prefix) {
					t.Fatalf("SOUNDNESS: %q matches %q (escape %q) but does not begin with "+
						"the asserted prefix %q, so that prefix is TOO LONG — a range built "+
						"from it would drop this row\n  %s",
						m, c.pattern, c.escape, c.prefix, c.why)
				}
			}

			// MAXIMALITY. Fails when prefix is too short, including "".
			n := len(c.prefix)
			if byteAt(c.matchA, n) == byteAt(c.matchB, n) {
				t.Fatalf("MAXIMALITY: %q and %q both match %q (escape %q) and agree at byte "+
					"offset %d, so THESE TWO share a longer prefix than the asserted %q and "+
					"no longer straddle its boundary. That does not by itself mean every "+
					"match shares a longer prefix — two agreeing witnesses cannot show that "+
					"— so either the asserted prefix is TOO SHORT or the witnesses need "+
					"re-picking to diverge at it again\n  %s",
					c.matchA, c.matchB, c.pattern, c.escape, n, c.prefix, c.why)
			}

			// RESIDUAL NECESSITY. See TODO.md CQ-33.
			if !strings.HasPrefix(c.rejectInRange, c.prefix) {
				t.Fatalf("setup is wrong: %q is not inside the byte-prefix range of %q, so it "+
					"witnesses nothing about the residual", c.rejectInRange, c.prefix)
			}
			if LikeMatch(c.pattern, c.rejectInRange, c.escape) {
				t.Fatalf("RESIDUAL: LikeMatch(%q, %q, escape %q) = true, want false — this "+
					"subject is inside the byte-prefix range of %q, so its rejection is what "+
					"makes the range a STRICT superset and the residual LIKE mandatory\n  %s",
					c.pattern, c.rejectInRange, c.escape, c.prefix, c.why)
			}
		})
	}
}

// byteAt returns s[i], or -1 when i is past the end, so that "one subject ends
// exactly at the prefix boundary" counts as divergence from one that continues.
func byteAt(s string, i int) int {
	if i >= len(s) {
		return -1
	}
	return int(s[i])
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
