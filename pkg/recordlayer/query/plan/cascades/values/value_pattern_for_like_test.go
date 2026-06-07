package values

import (
	"regexp"
	"testing"
)

func TestPatternForLikeValue_Type(t *testing.T) {
	t.Parallel()
	v := NewPatternForLikeValue(LiteralValue("a%"), LiteralValue(nil))
	if !v.Type().Equals(NotNullString) {
		t.Fatalf("Type = %v, want NotNullString", v.Type())
	}
}

func TestPatternForLikeValue_NoEscape(t *testing.T) {
	t.Parallel()
	cases := []struct {
		pat, want string
	}{
		{"abc", "^abc$"},
		{"abc%", "^abc.*$"},
		{"a_c", "^a.c$"},
		{"%abc%", "^.*abc.*$"},
		{"a.b", `^a\.b$`},
		{"$end", `^\$end$`},
		{"a+b", `^a\+b$`},
		{`a\b`, `^a\\b$`},
	}
	for _, tc := range cases {
		v := NewPatternForLikeValue(LiteralValue(tc.pat), LiteralValue(nil))
		if got := mustEvalForTest(v, nil); got != tc.want {
			t.Errorf("Evaluate(%q) = %q, want %q", tc.pat, got, tc.want)
		}
	}
}

func TestPatternForLikeValue_WithEscape(t *testing.T) {
	t.Parallel()
	// `\%` (with `\` as escape char) → literal `%`.
	v := NewPatternForLikeValue(LiteralValue(`a\%b`), LiteralValue(`\`))
	got := mustEvalForTest(v, nil)
	want := `^a%b$`
	if got != want {
		t.Fatalf("Evaluate(`a\\%%b`, esc=`\\`) = %q, want %q", got, want)
	}
	// `\_` → literal `_`.
	v2 := NewPatternForLikeValue(LiteralValue(`a\_b`), LiteralValue(`\`))
	got2 := mustEvalForTest(v2, nil)
	want2 := `^a_b$`
	if got2 != want2 {
		t.Fatalf("Evaluate(`a\\_b`, esc=`\\`) = %q, want %q", got2, want2)
	}
	// Bare `_` and `%` after escape rule still expand normally.
	v3 := NewPatternForLikeValue(LiteralValue("a%b_c"), LiteralValue(`\`))
	got3 := mustEvalForTest(v3, nil)
	want3 := `^a.*b.c$`
	if got3 != want3 {
		t.Fatalf("Evaluate(`a%%b_c`, esc=`\\`) = %q, want %q", got3, want3)
	}
}

func TestPatternForLikeValue_NullPattern(t *testing.T) {
	t.Parallel()
	v := NewPatternForLikeValue(LiteralValue(nil), LiteralValue(nil))
	if got := mustEvalForTest(v, nil); got != nil {
		t.Fatalf("Evaluate(NULL pattern) = %v, want nil", got)
	}
}

func TestPatternForLikeValue_NullEscapeIsNoEscape(t *testing.T) {
	t.Parallel()
	// NULL escape → standard transformation (matches Java contract).
	v := NewPatternForLikeValue(LiteralValue("a%"), LiteralValue(nil))
	if got := mustEvalForTest(v, nil); got != "^a.*$" {
		t.Fatalf("Evaluate(NULL escape) = %v, want ^a.*$", got)
	}
}

func TestPatternForLikeValue_MultiCharEscapeReturnsNil(t *testing.T) {
	t.Parallel()
	// Java throws SemanticException for non-single-char escape; Go
	// surfaces nil from the evaluator (planner is expected to check).
	v := NewPatternForLikeValue(LiteralValue("a%"), LiteralValue("xx"))
	if got := mustEvalForTest(v, nil); got != nil {
		t.Fatalf("Evaluate(2-char escape) = %v, want nil", got)
	}
}

func TestPatternForLikeValue_EmptyEscapeReturnsNil(t *testing.T) {
	t.Parallel()
	v := NewPatternForLikeValue(LiteralValue("a%"), LiteralValue(""))
	if got := mustEvalForTest(v, nil); got != nil {
		t.Fatalf("Evaluate(empty escape) = %v, want nil", got)
	}
}

func TestPatternForLikeValue_Children(t *testing.T) {
	t.Parallel()
	pat := LiteralValue("a")
	esc := LiteralValue("\\")
	v := NewPatternForLikeValue(pat, esc)
	cs := v.Children()
	if len(cs) != 2 || cs[0] != pat || cs[1] != esc {
		t.Fatalf("Children = %v, want [pat, esc]", cs)
	}
}

func TestPatternForLikeValue_RegexCompiles(t *testing.T) {
	t.Parallel()
	// The produced strings must be valid Go regex syntax — a basic
	// sanity check that the metachar escape table is correct (Go's
	// regexp.MustCompile rejects malformed patterns).
	cases := []string{
		"abc%",
		"a_c",
		"a.b",
		"a+b",
		"a*b",
		"$money",
		`a\b`,
		"a[b]c",
		"a{1,2}",
		"a(b)c",
	}
	for _, p := range cases {
		v := NewPatternForLikeValue(LiteralValue(p), LiteralValue(nil))
		got, ok := mustEvalForTest(v, nil).(string)
		if !ok {
			t.Errorf("Evaluate(%q) returned non-string", p)
			continue
		}
		if _, err := regexp.Compile(got); err != nil {
			t.Errorf("regexp.Compile(%q) from %q: %v", got, p, err)
		}
	}
}
