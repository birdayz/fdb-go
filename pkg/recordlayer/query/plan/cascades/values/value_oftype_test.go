package values

import "testing"

func TestOfTypeValue_BoolMatchesBool(t *testing.T) {
	t.Parallel()
	v := NewOfTypeValue(LiteralValue(true), NullableBoolean)
	if got := mustEvaluate(v, nil); got != true {
		t.Fatalf("OfType(true, BOOL) = %v, want true", got)
	}
}

func TestOfTypeValue_StringMatchesString(t *testing.T) {
	t.Parallel()
	v := NewOfTypeValue(LiteralValue("hi"), NullableString)
	if got := mustEvaluate(v, nil); got != true {
		t.Fatalf("OfType('hi', STRING) = %v, want true", got)
	}
}

func TestOfTypeValue_StringNotMatchesInt(t *testing.T) {
	t.Parallel()
	v := NewOfTypeValue(LiteralValue("hi"), NullableLong)
	if got := mustEvaluate(v, nil); got != false {
		t.Fatalf("OfType('hi', LONG) = %v, want false", got)
	}
}

func TestOfTypeValue_NullChildAgainstNullableType(t *testing.T) {
	t.Parallel()
	// Java conformance: NULL IS OF TYPE T returns T.isNullable().
	// NullableBoolean is nullable → expect true.
	v := NewOfTypeValue(LiteralValue(nil), NullableBoolean)
	if got := mustEvaluate(v, nil); got != true {
		t.Fatalf("OfType(NULL, NullableBoolean) = %v, want true (Java conformance: NULL of nullable type is OK)", got)
	}
}

func TestOfTypeValue_NullChildAgainstNotNullType(t *testing.T) {
	t.Parallel()
	// Java conformance: NULL IS OF TYPE T returns T.isNullable().
	// NotNullBoolean is non-nullable → expect false.
	v := NewOfTypeValue(LiteralValue(nil), NotNullBoolean)
	if got := mustEvaluate(v, nil); got != false {
		t.Fatalf("OfType(NULL, NotNullBoolean) = %v, want false (Java: NULL of non-nullable type rejected)", got)
	}
}

func TestOfTypeValue_TypeIsNullableBool(t *testing.T) {
	t.Parallel()
	v := NewOfTypeValue(LiteralValue(int64(1)), NullableLong)
	if v.Type() != NullableBoolean {
		t.Fatalf("Type=%v, want NullableBoolean", v.Type())
	}
}

// TestOfTypeValue_PrimitiveStrict_NoPromotion mirrors Java's
// OfTypeValueTest: `OfType(42 (int), LONG)` returns false even
// though INT is promotable to LONG in other Cascades contexts.
// Pinned for Java conformance (per "Java conformance is absolute
// king").
func TestOfTypeValue_PrimitiveStrict_NoPromotion(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		val  any
		typ  Type
		want any
	}{
		{"int32 vs INT", int32(42), NotNullInt, true},
		{"int32 vs LONG", int32(42), NotNullLong, false},
		{"int64 vs LONG", int64(42), NotNullLong, true},
		{"int64 vs INT", int64(42), NotNullInt, false},
		{"float32 vs FLOAT", float32(42.5), NotNullFloat, true},
		{"float32 vs DOUBLE", float32(42.5), NotNullDouble, false},
		{"float64 vs DOUBLE", 42.5, NotNullDouble, true},
		{"float64 vs FLOAT", 42.5, NotNullFloat, false},
		{"string vs STRING", "hi", NotNullString, true},
		{"bool vs BOOLEAN", true, NotNullBoolean, true},
	}
	for _, c := range cases {
		v := NewOfTypeValue(LiteralValue(c.val), c.typ)
		if got := mustEvaluate(v, nil); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestOfTypeValue_Children(t *testing.T) {
	t.Parallel()
	child := LiteralValue(int64(42))
	v := NewOfTypeValue(child, NullableLong)
	cs := v.Children()
	if len(cs) != 1 || cs[0] != child {
		t.Fatalf("Children = %v, want [child]", cs)
	}
}

// BenchmarkOfTypeValue_Evaluate pins the perf of the conformance-
// pinned strict primitive match. Hot path for type-guard rules.
func BenchmarkOfTypeValue_Evaluate(b *testing.B) {
	v := NewOfTypeValue(LiteralValue(int64(42)), NullableLong)
	for i := 0; i < b.N; i++ {
		_ = mustEvaluate(v, nil)
	}
}
