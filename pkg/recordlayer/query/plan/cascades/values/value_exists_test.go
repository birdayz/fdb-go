package values

import "testing"

func TestExistsValue_LeafShape(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("subq")
	v := NewExistsValue(alias)
	if len(v.Children()) != 0 {
		t.Fatal("ExistsValue should be a leaf")
	}
	if v.Alias != alias {
		t.Fatalf("Alias = %v, want %v", v.Alias, alias)
	}
}

func TestExistsValue_TypeIsNotNullBoolean(t *testing.T) {
	t.Parallel()
	v := NewExistsValue(NamedCorrelationIdentifier("x"))
	if !v.Type().Equals(NotNullBoolean) {
		t.Fatalf("Type=%v, want NotNullBoolean", v.Type())
	}
}

func TestExistsValue_EvaluatePanics(t *testing.T) {
	t.Parallel()
	v := NewExistsValue(NamedCorrelationIdentifier("x"))
	defer func() {
		if recover() == nil {
			t.Fatal("ExistsValue.Evaluate should panic")
		}
	}()
	_, _ = v.Evaluate(nil)
}

func TestExistsValue_CorrelatedToAlias(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("subq")
	v := NewExistsValue(alias)
	cs := v.GetCorrelatedTo()
	if len(cs) != 1 {
		t.Fatalf("CorrelatedTo size = %d, want 1", len(cs))
	}
	if _, ok := cs[alias]; !ok {
		t.Fatalf("CorrelatedTo missing alias %v", alias)
	}
}
