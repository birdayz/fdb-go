package values

import (
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
		{"trailing escape malformed", `ab\`, "ab", '\\', false},
		{"escaped escape char", `a\\b`, `a\b`, '\\', true},
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
