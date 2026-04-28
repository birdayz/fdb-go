package values

import "testing"

func TestOfTypeValue_BoolMatchesBool(t *testing.T) {
	t.Parallel()
	v := NewOfTypeValue(LiteralValue(true), NullableBoolean)
	if got := v.Evaluate(nil); got != true {
		t.Fatalf("OfType(true, BOOL) = %v, want true", got)
	}
}

func TestOfTypeValue_StringMatchesString(t *testing.T) {
	t.Parallel()
	v := NewOfTypeValue(LiteralValue("hi"), NullableString)
	if got := v.Evaluate(nil); got != true {
		t.Fatalf("OfType('hi', STRING) = %v, want true", got)
	}
}

func TestOfTypeValue_StringNotMatchesInt(t *testing.T) {
	t.Parallel()
	v := NewOfTypeValue(LiteralValue("hi"), NullableLong)
	if got := v.Evaluate(nil); got != false {
		t.Fatalf("OfType('hi', LONG) = %v, want false", got)
	}
}

func TestOfTypeValue_NullChildIsNil(t *testing.T) {
	t.Parallel()
	v := NewOfTypeValue(LiteralValue(nil), NullableBoolean)
	if got := v.Evaluate(nil); got != nil {
		t.Fatalf("OfType(NULL, BOOL) = %v, want nil (UNKNOWN)", got)
	}
}

func TestOfTypeValue_TypeIsNullableBool(t *testing.T) {
	t.Parallel()
	v := NewOfTypeValue(LiteralValue(int64(1)), NullableLong)
	if v.Type() != NullableBoolean {
		t.Fatalf("Type=%v, want NullableBoolean", v.Type())
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
