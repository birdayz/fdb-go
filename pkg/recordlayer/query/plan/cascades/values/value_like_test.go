package values

import "testing"

func TestLikeOperatorValue_PercentWildcardPrefix(t *testing.T) {
	t.Parallel()
	v := NewLikeOperatorValue(LiteralValue("hello world"), LiteralValue("hello%"))
	if got := v.Evaluate(nil); got != true {
		t.Fatalf("'hello world' LIKE 'hello%%' = %v, want true", got)
	}
}

func TestLikeOperatorValue_PercentWildcardSuffix(t *testing.T) {
	t.Parallel()
	v := NewLikeOperatorValue(LiteralValue("hello world"), LiteralValue("%world"))
	if got := v.Evaluate(nil); got != true {
		t.Fatalf("'hello world' LIKE '%%world' = %v, want true", got)
	}
}

func TestLikeOperatorValue_UnderscoreWildcard(t *testing.T) {
	t.Parallel()
	v := NewLikeOperatorValue(LiteralValue("abc"), LiteralValue("a_c"))
	if got := v.Evaluate(nil); got != true {
		t.Fatalf("'abc' LIKE 'a_c' = %v, want true", got)
	}
	v2 := NewLikeOperatorValue(LiteralValue("abcd"), LiteralValue("a_c"))
	if got := v2.Evaluate(nil); got != false {
		t.Fatalf("'abcd' LIKE 'a_c' = %v, want false (one underscore = exactly 1 char)", got)
	}
}

func TestLikeOperatorValue_LiteralMatch(t *testing.T) {
	t.Parallel()
	v := NewLikeOperatorValue(LiteralValue("exact"), LiteralValue("exact"))
	if got := v.Evaluate(nil); got != true {
		t.Fatalf("'exact' LIKE 'exact' = %v, want true", got)
	}
}

func TestLikeOperatorValue_NoMatch(t *testing.T) {
	t.Parallel()
	v := NewLikeOperatorValue(LiteralValue("hello"), LiteralValue("world"))
	if got := v.Evaluate(nil); got != false {
		t.Fatalf("'hello' LIKE 'world' = %v, want false", got)
	}
}

func TestLikeOperatorValue_RegexMetacharsEscaped(t *testing.T) {
	t.Parallel()
	// '.' in pattern is NOT a wildcard in SQL LIKE; should match
	// only a literal dot.
	v := NewLikeOperatorValue(LiteralValue("a.c"), LiteralValue("a.c"))
	if got := v.Evaluate(nil); got != true {
		t.Fatalf("'a.c' LIKE 'a.c' = %v, want true (literal dot)", got)
	}
	v2 := NewLikeOperatorValue(LiteralValue("axc"), LiteralValue("a.c"))
	if got := v2.Evaluate(nil); got != false {
		t.Fatalf("'axc' LIKE 'a.c' = %v, want false (dot is literal not regex .)", got)
	}
}

func TestLikeOperatorValue_NullProbe(t *testing.T) {
	t.Parallel()
	v := NewLikeOperatorValue(LiteralValue(nil), LiteralValue("abc"))
	if got := v.Evaluate(nil); got != nil {
		t.Fatalf("NULL LIKE 'abc' = %v, want nil (UNKNOWN)", got)
	}
}

func TestLikeOperatorValue_NullPattern(t *testing.T) {
	t.Parallel()
	v := NewLikeOperatorValue(LiteralValue("abc"), LiteralValue(nil))
	if got := v.Evaluate(nil); got != nil {
		t.Fatalf("'abc' LIKE NULL = %v, want nil (UNKNOWN)", got)
	}
}

func TestLikeOperatorValue_NonStringProbe(t *testing.T) {
	t.Parallel()
	v := NewLikeOperatorValue(LiteralValue(int64(1)), LiteralValue("abc"))
	if got := v.Evaluate(nil); got != nil {
		t.Fatalf("1 LIKE 'abc' = %v, want nil (type-degraded)", got)
	}
}

func TestLikeOperatorValue_TypeIsNullableBool(t *testing.T) {
	t.Parallel()
	v := NewLikeOperatorValue(LiteralValue("a"), LiteralValue("a"))
	if v.Type() != NullableBoolean {
		t.Fatalf("Type=%v, want NullableBoolean", v.Type())
	}
}

func TestLikeOperatorValue_PatternToRegex(t *testing.T) {
	t.Parallel()
	cases := []struct {
		pattern, want string
	}{
		{"abc", `^abc$`},
		{"abc%", `^abc.*$`},
		{"%abc", `^.*abc$`},
		{"a_b", `^a.b$`},
		{"a.b", `^a\.b$`},
		{"$test^", `^\$test\^$`},
		{`a\b`, `^a\\b$`},
	}
	for _, c := range cases {
		got := likePatternToRegex(c.pattern)
		if got != c.want {
			t.Errorf("likePatternToRegex(%q) = %q, want %q", c.pattern, got, c.want)
		}
	}
}
