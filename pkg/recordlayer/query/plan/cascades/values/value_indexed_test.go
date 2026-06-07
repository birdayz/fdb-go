package values

import "testing"

func TestIndexedValue_LeafShape(t *testing.T) {
	t.Parallel()
	v := NewIndexedValue(NotNullLong)
	if got := len(v.Children()); got != 0 {
		t.Fatalf("IndexedValue children = %d, want 0", got)
	}
	if v.Name() != "indexed" {
		t.Fatalf("Name = %q", v.Name())
	}
}

func TestIndexedValue_TypePreserved(t *testing.T) {
	t.Parallel()
	v := NewIndexedValue(NullableString)
	if !v.Type().Equals(NullableString) {
		t.Fatalf("Type=%v, want NullableString", v.Type())
	}
}

func TestIndexedValue_NilTypeFallsBackToUnknown(t *testing.T) {
	t.Parallel()
	v := NewIndexedValue(nil)
	if v.Type() != UnknownType {
		t.Fatalf("Type=%v, want UnknownType", v.Type())
	}
}

func TestIndexedValue_EvaluatePanics(t *testing.T) {
	t.Parallel()
	v := NewIndexedValue(NotNullLong)
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("IndexedValue.Evaluate should panic")
		}
	}()
	mustEvaluate(v, nil)
}
