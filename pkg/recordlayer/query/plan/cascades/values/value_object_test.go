package values

import "testing"

func TestObjectValue_LeafShape(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("x")
	v := NewObjectValue(alias, NotNullLong)
	if len(v.Children()) != 0 {
		t.Fatal("ObjectValue should be a leaf")
	}
	if v.Alias != alias {
		t.Fatalf("Alias=%v, want %v", v.Alias, alias)
	}
}

func TestObjectValue_TypePreserved(t *testing.T) {
	t.Parallel()
	v := NewObjectValue(NamedCorrelationIdentifier("x"), NullableString)
	if !v.Type().Equals(NullableString) {
		t.Fatalf("Type=%v, want NullableString", v.Type())
	}
}

func TestObjectValue_NilTypeFallsBackToUnknown(t *testing.T) {
	t.Parallel()
	v := NewObjectValue(NamedCorrelationIdentifier("x"), nil)
	if v.Type() != UnknownType {
		t.Fatalf("Type=%v, want UnknownType", v.Type())
	}
}

func TestObjectValue_EvaluateReturnsNil(t *testing.T) {
	t.Parallel()
	v := NewObjectValue(NamedCorrelationIdentifier("x"), NotNullLong)
	if got := mustEvaluate(v, nil); got != nil {
		t.Fatalf("Evaluate = %v, want nil (placeholder)", got)
	}
}

func TestObjectValue_CorrelatedToAlias(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("x")
	v := NewObjectValue(alias, NotNullLong)
	cs := v.GetCorrelatedTo()
	if len(cs) != 1 {
		t.Fatalf("CorrelatedTo size = %d, want 1", len(cs))
	}
	if _, ok := cs[alias]; !ok {
		t.Fatalf("CorrelatedTo missing alias %v", alias)
	}
}
