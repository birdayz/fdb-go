package values

import "testing"

// stubConstantDeref implements the ConstantDeref capability for
// testing.
type stubConstantDeref struct {
	values map[constantKey]any
}

type constantKey struct {
	alias      CorrelationIdentifier
	constantID string
}

func (s *stubConstantDeref) DereferenceConstant(alias CorrelationIdentifier, constantID string) any {
	return s.values[constantKey{alias: alias, constantID: constantID}]
}

func TestConstantObjectValue_LeafShape(t *testing.T) {
	t.Parallel()
	v := NewConstantObjectValue(NamedCorrelationIdentifier("a"), "c1", NotNullLong)
	if len(v.Children()) != 0 {
		t.Fatal("ConstantObjectValue should be a leaf")
	}
	if v.Alias.Name() != "a" {
		t.Fatalf("Alias=%q", v.Alias.Name())
	}
	if v.ConstantID != "c1" {
		t.Fatalf("ConstantID=%q", v.ConstantID)
	}
}

func TestConstantObjectValue_TypePreserved(t *testing.T) {
	t.Parallel()
	v := NewConstantObjectValue(NamedCorrelationIdentifier("a"), "c1", NullableString)
	if !v.Type().Equals(NullableString) {
		t.Fatalf("Type=%v, want NullableString", v.Type())
	}
}

func TestConstantObjectValue_NilTypeFallsBackToUnknown(t *testing.T) {
	t.Parallel()
	v := NewConstantObjectValue(NamedCorrelationIdentifier("a"), "c1", nil)
	if v.Type() != UnknownType {
		t.Fatalf("Type=%v, want UnknownType", v.Type())
	}
}

func TestConstantObjectValue_EvaluateNoDereferReturnsNil(t *testing.T) {
	t.Parallel()
	v := NewConstantObjectValue(NamedCorrelationIdentifier("a"), "c1", NotNullLong)
	if got := v.Evaluate(nil); got != nil {
		t.Fatalf("Evaluate without ConstantDeref = %v, want nil", got)
	}
	if got := v.Evaluate("not a deref"); got != nil {
		t.Fatalf("Evaluate with non-ConstantDeref = %v, want nil", got)
	}
}

func TestConstantObjectValue_EvaluateLooksUpBinding(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("a")
	v := NewConstantObjectValue(alias, "c1", NotNullLong)
	stub := &stubConstantDeref{values: map[constantKey]any{
		{alias: alias, constantID: "c1"}: int64(42),
	}}
	if got := v.Evaluate(stub); got != int64(42) {
		t.Fatalf("Evaluate = %v, want int64(42)", got)
	}
}

func TestConstantObjectValue_EvaluateMissingBinding(t *testing.T) {
	t.Parallel()
	v := NewConstantObjectValue(NamedCorrelationIdentifier("a"), "c1", NotNullLong)
	stub := &stubConstantDeref{values: map[constantKey]any{}}
	if got := v.Evaluate(stub); got != nil {
		t.Fatalf("Evaluate missing binding = %v, want nil", got)
	}
}

func TestConstantObjectValue_CorrelatedToAlias(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("a")
	v := NewConstantObjectValue(alias, "c1", NotNullLong)
	cs := v.GetCorrelatedTo()
	if len(cs) != 1 {
		t.Fatalf("CorrelatedTo size = %d, want 1", len(cs))
	}
	if _, ok := cs[alias]; !ok {
		t.Fatalf("CorrelatedTo missing alias %v", alias)
	}
}
