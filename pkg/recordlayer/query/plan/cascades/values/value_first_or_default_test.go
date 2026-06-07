package values

import "testing"

func TestFirstOrDefaultValue_NonEmptyReturnsFirst(t *testing.T) {
	t.Parallel()
	arr := LiteralValue([]any{int64(10), int64(20), int64(30)})
	def := LiteralValue(int64(99))
	v := NewFirstOrDefaultValue(arr, def, NotNullLong)
	if got := mustEvalForTest(v, nil); got != int64(10) {
		t.Fatalf("FIRST_OR_DEFAULT([10,20,30], 99) = %v, want 10", got)
	}
}

func TestFirstOrDefaultValue_EmptyReturnsDefault(t *testing.T) {
	t.Parallel()
	arr := LiteralValue([]any{})
	def := LiteralValue(int64(99))
	v := NewFirstOrDefaultValue(arr, def, NotNullLong)
	if got := mustEvalForTest(v, nil); got != int64(99) {
		t.Fatalf("FIRST_OR_DEFAULT([], 99) = %v, want 99", got)
	}
}

func TestFirstOrDefaultValue_NullArrayReturnsNil(t *testing.T) {
	t.Parallel()
	// CONFORMANCE: Java returns NULL (not the default) for a NULL
	// array — the default is for empty arrays only.
	v := NewFirstOrDefaultValue(LiteralValue(nil), LiteralValue(int64(99)), NotNullLong)
	if got := mustEvalForTest(v, nil); got != nil {
		t.Fatalf("FIRST_OR_DEFAULT(NULL, 99) = %v, want nil (Java conformance)", got)
	}
}

func TestFirstOrDefaultValue_NonSliceReturnsNil(t *testing.T) {
	t.Parallel()
	v := NewFirstOrDefaultValue(LiteralValue("not-a-list"), LiteralValue(int64(99)), NotNullLong)
	if got := mustEvalForTest(v, nil); got != nil {
		t.Fatalf("FIRST_OR_DEFAULT non-slice = %v, want nil", got)
	}
}

func TestFirstOrDefaultValue_TypePreserved(t *testing.T) {
	t.Parallel()
	v := NewFirstOrDefaultValue(LiteralValue([]any{}), LiteralValue("def"), NotNullString)
	if !v.Type().Equals(NotNullString) {
		t.Fatalf("Type=%v, want NotNullString", v.Type())
	}
}

func TestFirstOrDefaultValue_Children(t *testing.T) {
	t.Parallel()
	arr := LiteralValue([]any{int64(1)})
	def := LiteralValue(int64(99))
	v := NewFirstOrDefaultValue(arr, def, NotNullLong)
	cs := v.Children()
	if len(cs) != 2 || cs[0] != arr || cs[1] != def {
		t.Fatalf("Children = %v, want [arr, def]", cs)
	}
}
