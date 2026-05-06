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

// likePatternToRegex was removed when LikeOperatorValue was
// re-routed through LikeMatch (shared with predicates.likeMatch).
// The regex translation existed for the Value-layer-only path that
// no longer exists. LikeMatch is the conformance-pinned matcher.

// BenchmarkLikeMatch_Simple pins the perf of the canonical
// SQL LIKE matcher. Both QueryPredicate-layer ComparisonLike and
// Value-layer LikeOperatorValue route through this; perf
// regression here regresses both layers.
func BenchmarkLikeMatch_Simple(b *testing.B) {
	for i := 0; i < b.N; i++ {
		_ = LikeMatch("hello%", "hello world", 0)
	}
}

// BenchmarkLikeMatch_Wildcards exercises the % backtrack.
func BenchmarkLikeMatch_Wildcards(b *testing.B) {
	for i := 0; i < b.N; i++ {
		_ = LikeMatch("%abc%def%", "xxabcyydefzz", 0)
	}
}

// BenchmarkLikeMatch_LiteralOnly — no wildcards, just literal
// match. Should be the fastest path.
func BenchmarkLikeMatch_LiteralOnly(b *testing.B) {
	for i := 0; i < b.N; i++ {
		_ = LikeMatch("hello world", "hello world", 0)
	}
}
