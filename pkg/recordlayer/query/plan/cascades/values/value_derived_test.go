package values

import "testing"

func TestDerivedValue_PreservesChildren(t *testing.T) {
	t.Parallel()
	c1 := LiteralValue(int64(1))
	c2 := LiteralValue("hi")
	v := NewDerivedValue([]Value{c1, c2})
	cs := v.Children()
	if len(cs) != 2 || cs[0] != c1 || cs[1] != c2 {
		t.Fatalf("children = %v, want [c1, c2]", cs)
	}
}

func TestDerivedValue_NewWithType(t *testing.T) {
	t.Parallel()
	v := NewDerivedValueWithType(nil, NotNullLong)
	if !v.Type().Equals(NotNullLong) {
		t.Fatalf("Type=%v, want NotNullLong", v.Type())
	}
}

func TestDerivedValue_DefaultUnknownType(t *testing.T) {
	t.Parallel()
	v := NewDerivedValue(nil)
	if v.Type() != UnknownType {
		t.Fatalf("Type=%v, want UnknownType", v.Type())
	}
}

func TestDerivedValue_EvaluatePanics(t *testing.T) {
	t.Parallel()
	v := NewDerivedValue(nil)
	defer func() {
		if recover() == nil {
			t.Fatal("DerivedValue.Evaluate should panic")
		}
	}()
	mustEvalForTest(v, nil)
}
