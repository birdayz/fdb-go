package values

import (
	"errors"
	"testing"
	"unicode/utf16"
)

func strp(s string) *string { return &s }

// TestMatchLike pins Java 4.14's matchLike (LikeOperatorValue.java:145-241):
// whole-string matching over UTF-16 units, wildcards crossing line terminators,
// `_` consuming a surrogate pair, and escapes.
func TestMatchLike(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		text, pattern string
		escape        *string
		want          bool
	}{
		{"abc", "abc", nil, true},
		{"abc", "ab", nil, false},
		{"abc", "a%", nil, true},
		{"abc", "%c", nil, true},
		{"abc", "a_c", nil, true},
		{"", "", nil, true},
		{"a", "", nil, false},
		{"", "%", nil, true},
		{"", "_", nil, false},
		{"aXbXc", "a%b%c", nil, true},
		{"aaaa", "%aaaa", nil, true},
		{"aaa", "%aaaa", nil, false},
		// Line terminators are ordinary characters.
		{"a\nb", "a%b", nil, true},
		{"a\nb", "a_b", nil, true},
		{"ab\n", "ab", nil, false},
		{"ab\n", "ab%", nil, true},
		{"a\r\u2028b", "a__b", nil, true},
		// `_` consumes one code point, a surrogate pair included.
		{"😀", "_", nil, true},
		{"😀", "__", nil, false},
		{"a😀b", "a_b", nil, true},
		{"😀x", "%_", nil, true},
		{"😀", "😀", nil, true},
		{"é", "_", nil, true},
		// Escapes.
		{"a%b", `a\%b`, strp(`\`), true},
		{"aXb", `a\%b`, strp(`\`), false},
		{"a_b", `a\_b`, strp(`\`), true},
		{"aXb", `a\_b`, strp(`\`), false},
		{`a\b`, `a\\b`, strp(`\`), true},
		{"a%", `a!%`, strp("!"), true},
		{"50%", `%!%`, strp("!"), true},
		{"a%b", "aé%b", strp("é"), true},
	} {
		got, err := MatchLike(tc.text, tc.pattern, tc.escape)
		if err != nil || got != tc.want {
			t.Errorf("MatchLike(%q, %q, %v) = %v, %v; want %v", tc.text, tc.pattern, tc.escape, got, err, tc.want)
		}
	}
}

func TestMatchLike_Errors(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		pattern, escape string
		want            LikeErrorKind
	}{
		{`a\`, `\`, LikeInvalidEscapeSequence},
		{`a\b`, `\`, LikeInvalidEscapeSequence},
		{"a", "ab", LikeEscapeNotSingleChar},
		{"a", "", LikeEscapeNotSingleChar},
		{"a", "😀", LikeEscapeNotSingleChar},
		{"a", "%", LikeEscapeConflict},
		{"a", "_", LikeEscapeConflict},
	} {
		_, err := MatchLike("abc", tc.pattern, &tc.escape)
		var le *LikeError
		if !errors.As(err, &le) || le.Kind != tc.want {
			t.Errorf("MatchLike(%q ESCAPE %q) error %v, want kind %d", tc.pattern, tc.escape, err, tc.want)
		}
	}
}

// TestPatternForLikeValue_Evaluate: the escape is validated whenever present,
// even with a NULL pattern; the pattern only when both are present; absent
// fields stay absent (PatternForLikeValue.java:115-131).
func TestPatternForLikeValue_Evaluate(t *testing.T) {
	t.Parallel()
	eval := func(pattern, escape any) (any, error) {
		return NewPatternForLikeValue(LiteralValue(pattern), LiteralValue(escape)).Evaluate(nil)
	}
	got, err := eval("a%", nil)
	if lp, ok := got.(LikePattern); err != nil || !ok || lp.Pattern == nil || *lp.Pattern != "a%" || lp.Escape != nil {
		t.Errorf("no escape: %#v, %v", got, err)
	}
	got, err = eval(nil, nil)
	if lp, ok := got.(LikePattern); err != nil || !ok || lp.Pattern != nil || lp.Escape != nil {
		t.Errorf("NULL pattern: %#v, %v", got, err)
	}
	if _, err := eval(nil, "ab"); !isLikeError(err, LikeEscapeNotSingleChar) {
		t.Errorf("a bad escape with a NULL pattern: %v", err)
	}
	if _, err := eval(`a\b`, `\`); !isLikeError(err, LikeInvalidEscapeSequence) {
		t.Errorf("a bad escape sequence: %v", err)
	}
	if got, err := LikeOperation("abc", LikePattern{}); got != nil || err != nil {
		t.Errorf("an absent pattern is NULL: %v, %v", got, err)
	}
	if got, err := LikeOperation(nil, LikePattern{Pattern: strp("%")}); got != nil || err != nil {
		t.Errorf("a NULL operand is NULL: %v, %v", got, err)
	}
	if _, ok := NewPatternForLikeValue(LiteralValue("a"), LiteralValue(nil)).Type().(*RecordType); !ok {
		t.Error("the pattern's type is not a record")
	}
	if _, err := NewPatternForLikeValueChecked(&ConstantValue{Value: int64(1), Typ: NotNullLong}, LiteralValue(nil)); !isLikeError(err, LikeOperandNotString) {
		t.Errorf("a numeric pattern: %v", err)
	}
	like := NewLikeOperatorValue(LiteralValue("abc"), NewPatternForLikeValue(LiteralValue("a_c"), LiteralValue(nil)))
	if got, err := like.Evaluate(nil); got != true || err != nil {
		t.Errorf("LikeOperatorValue: %v, %v", got, err)
	}
}

func isLikeError(err error, kind LikeErrorKind) bool {
	var le *LikeError
	return errors.As(err, &le) && le.Kind == kind
}

// TestJavaUTF16_MaximalSubparts: an invalid stored string decodes as Java's
// lenient UTF-8 decoding does, one U+FFFD per maximal subpart (the Unicode
// standard's own example, section 3.9, "U+FFFD Substitution of Maximal
// Subparts").
func TestJavaUTF16_MaximalSubparts(t *testing.T) {
	t.Parallel()
	in := string([]byte{0x61, 0xF1, 0x80, 0x80, 0xE1, 0x80, 0xC2, 0x62, 0x80, 0x63, 0x80, 0xBF, 0x64})
	want := utf16.Encode([]rune("a\uFFFD\uFFFD\uFFFDb\uFFFDc\uFFFD\uFFFDd"))
	got := javaUTF16(in)
	if string(utf16.Decode(got)) != string(utf16.Decode(want)) {
		t.Errorf("javaUTF16 = %q, want %q", string(utf16.Decode(got)), string(utf16.Decode(want)))
	}
	if ok, err := MatchLike(in, "a___b%", nil); !ok || err != nil {
		t.Errorf("three replacement characters between a and b: %v, %v", ok, err)
	}
}

// referenceLike is the declarative LIKE over UTF-16 units: `%` any run of
// units, `_` a surrogate pair when one starts here and one unit otherwise, an
// escape the next unit literally.
func referenceLike(text, pattern []uint16, hasEscape bool, escape uint16) bool {
	if len(pattern) == 0 {
		return len(text) == 0
	}
	pc := pattern[0]
	switch {
	case hasEscape && pc == escape:
		return len(text) > 0 && len(pattern) > 1 && text[0] == pattern[1] && referenceLike(text[1:], pattern[2:], hasEscape, escape)
	case pc == '%':
		for i := 0; i <= len(text); i++ {
			if referenceLike(text[i:], pattern[1:], hasEscape, escape) {
				return true
			}
		}
		return false
	case pc == '_':
		if len(text) == 0 {
			return false
		}
		n := 1
		if isHighSurrogateUnit(text[0]) && len(text) > 1 && isLowSurrogateUnit(text[1]) {
			n = 2
		}
		return referenceLike(text[n:], pattern[1:], hasEscape, escape)
	}
	return len(text) > 0 && text[0] == pc && referenceLike(text[1:], pattern[1:], hasEscape, escape)
}

// FuzzMatchLike checks MatchLike against the declarative reference over
// patterns whose escape sequences are valid.
func FuzzMatchLike(f *testing.F) {
	for _, s := range [][3]string{{"abc", "a%c", ""}, {"a😀b", "a_b", ""}, {"a%b", `a\%b`, `\`}, {"x\ny", "x_y", ""}, {"aaab", "%a_b", "!"}} {
		f.Add(s[0], s[1], s[2])
	}
	f.Fuzz(func(t *testing.T, text, pattern, escape string) {
		var esc *string
		var unit uint16
		if escape != "" {
			u, err := validateLikeEscape(escape)
			if err != nil {
				return
			}
			if validateLikePattern(utf16.Encode([]rune(pattern)), u) != nil {
				return
			}
			esc, unit = &escape, u
		}
		got, err := MatchLike(text, pattern, esc)
		if err != nil {
			t.Fatalf("MatchLike(%q, %q, %q): %v", text, pattern, escape, err)
		}
		if want := referenceLike(javaUTF16(text), utf16.Encode([]rune(pattern)), esc != nil, unit); got != want {
			t.Fatalf("MatchLike(%q, %q, %q) = %v, reference %v", text, pattern, escape, got, want)
		}
	})
}
