package values

import "testing"

func TestThrowsValue_LeafShape(t *testing.T) {
	t.Parallel()
	v := NewThrowsValue(NotNullLong)
	if len(v.Children()) != 0 {
		t.Fatal("ThrowsValue should be a leaf")
	}
}

func TestThrowsValue_TypePreserved(t *testing.T) {
	t.Parallel()
	v := NewThrowsValue(NullableString)
	if !v.Type().Equals(NullableString) {
		t.Fatalf("Type=%v, want NullableString", v.Type())
	}
}

func TestThrowsValue_EvaluatePanics(t *testing.T) {
	t.Parallel()
	v := NewThrowsValue(NotNullLong)
	defer func() {
		if recover() == nil {
			t.Fatal("ThrowsValue.Evaluate should panic")
		}
	}()
	mustEvaluate(v, nil)
}

func TestThrowsValue_NilTypeFallsBackToUnknown(t *testing.T) {
	t.Parallel()
	v := NewThrowsValue(nil)
	if v.Type() != UnknownType {
		t.Fatalf("Type=%v, want UnknownType", v.Type())
	}
}
