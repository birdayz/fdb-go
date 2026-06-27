package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestEmptyValueEquivalence(t *testing.T) {
	t.Parallel()
	eq := EmptyValueEquivalence()

	a := values.NamedCorrelationIdentifier("a")
	b := values.NamedCorrelationIdentifier("b")

	if eq.IsDefinedEqualAlias(a, b).Value {
		t.Fatal("empty equivalence should return false for all aliases")
	}
	if eq.IsDefinedEqualAlias(a, a).Value {
		t.Fatal("empty equivalence should return false even for same alias")
	}

	v1 := values.NewQuantifiedObjectValue(a)
	v2 := values.NewQuantifiedObjectValue(b)
	if eq.IsDefinedEqual(v1, v2).Value {
		t.Fatal("empty equivalence should return false for all values")
	}
}

func TestAliasMapValueEquivalence_MappedAliases(t *testing.T) {
	t.Parallel()
	src := values.NamedCorrelationIdentifier("src")
	tgt := values.NamedCorrelationIdentifier("tgt")

	am := AliasMapOfAliases(src, tgt)
	eq := NewAliasMapValueEquivalence(am)

	if !eq.IsDefinedEqualAlias(src, tgt).Value {
		t.Fatal("mapped aliases should be equal")
	}

	other := values.NamedCorrelationIdentifier("other")
	if eq.IsDefinedEqualAlias(src, other).Value {
		t.Fatal("unmapped aliases should not be equal")
	}
}

func TestAliasMapValueEquivalence_QOVValues(t *testing.T) {
	t.Parallel()
	a := values.NamedCorrelationIdentifier("a")
	b := values.NamedCorrelationIdentifier("b")

	am := AliasMapOfAliases(a, b)
	eq := NewAliasMapValueEquivalence(am)

	va := values.NewQuantifiedObjectValue(a)
	vb := values.NewQuantifiedObjectValue(b)

	if !eq.IsDefinedEqual(va, vb).Value {
		t.Fatal("QOV with mapped aliases should be equal")
	}

	vc := values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("c"))
	if eq.IsDefinedEqual(va, vc).Value {
		t.Fatal("QOV with unmapped alias should not be equal")
	}
}

func TestAliasMapValueEquivalence_NonQOVValues(t *testing.T) {
	t.Parallel()
	am := AliasMapOfAliases(
		values.NamedCorrelationIdentifier("a"),
		values.NamedCorrelationIdentifier("b"),
	)
	eq := NewAliasMapValueEquivalence(am)

	fv1 := &values.FieldValue{Field: "X"}
	fv2 := &values.FieldValue{Field: "X"}
	if eq.IsDefinedEqual(fv1, fv2).Value {
		t.Fatal("non-QOV values should not be equal via alias equivalence")
	}
}

func TestAliasMapValueEquivalence_NilAliasMap(t *testing.T) {
	t.Parallel()
	eq := NewAliasMapValueEquivalence(nil)

	a := values.NamedCorrelationIdentifier("a")
	b := values.NamedCorrelationIdentifier("b")
	if eq.IsDefinedEqualAlias(a, b).Value {
		t.Fatal("nil alias map should return false")
	}
}

func TestConstrainedBoolean_AlwaysTrue(t *testing.T) {
	t.Parallel()
	cb := AlwaysTrue()
	if !cb.Value {
		t.Fatal("AlwaysTrue should be true")
	}
	if cb.Constraint != nil {
		t.Fatal("AlwaysTrue should have nil constraint")
	}
}

func TestConstrainedBoolean_FalseValue(t *testing.T) {
	t.Parallel()
	cb := FalseValue()
	if cb.Value {
		t.Fatal("FalseValue should be false")
	}
}

func TestConstrainedBoolean_TrueWithConstraint(t *testing.T) {
	t.Parallel()
	c := Tautology()
	cb := TrueWithConstraint(c)
	if !cb.Value {
		t.Fatal("should be true")
	}
	if cb.Constraint != c {
		t.Fatal("should carry the constraint")
	}
}

func TestConstrainedBoolean_IsTrue_IsFalse(t *testing.T) {
	t.Parallel()
	tr := AlwaysTrue()
	fa := FalseValue()
	if !tr.IsTrue() {
		t.Fatal("AlwaysTrue should be true")
	}
	if tr.IsFalse() {
		t.Fatal("AlwaysTrue should not be false")
	}
	if fa.IsTrue() {
		t.Fatal("FalseValue should not be true")
	}
	if !fa.IsFalse() {
		t.Fatal("FalseValue should be false")
	}
}

func TestConstrainedBoolean_ComposeWithOther(t *testing.T) {
	t.Parallel()
	t.Run("true_true", func(t *testing.T) {
		t.Parallel()
		result := AlwaysTrue().ComposeWithOther(AlwaysTrue())
		if !result.IsTrue() {
			t.Fatal("true AND true should be true")
		}
	})
	t.Run("true_false", func(t *testing.T) {
		t.Parallel()
		result := AlwaysTrue().ComposeWithOther(FalseValue())
		if !result.IsFalse() {
			t.Fatal("true AND false should be false")
		}
	})
	t.Run("false_true", func(t *testing.T) {
		t.Parallel()
		result := FalseValue().ComposeWithOther(AlwaysTrue())
		if !result.IsFalse() {
			t.Fatal("false AND true should be false")
		}
	})
	t.Run("constrained_true", func(t *testing.T) {
		t.Parallel()
		c := Tautology()
		result := TrueWithConstraint(c).ComposeWithOther(AlwaysTrue())
		if !result.IsTrue() || result.Constraint != c {
			t.Fatal("constrained AND true should preserve constraint")
		}
	})
}

func TestConstrainedBoolean_Filter(t *testing.T) {
	t.Parallel()
	t.Run("false_short_circuits", func(t *testing.T) {
		t.Parallel()
		called := false
		result := FalseValue().Filter(func() ConstrainedBoolean {
			called = true
			return AlwaysTrue()
		})
		if !result.IsFalse() {
			t.Fatal("false.Filter should return false")
		}
		if called {
			t.Fatal("Filter should short-circuit on false")
		}
	})
	t.Run("true_evaluates", func(t *testing.T) {
		t.Parallel()
		result := AlwaysTrue().Filter(func() ConstrainedBoolean {
			return AlwaysTrue()
		})
		if !result.IsTrue() {
			t.Fatal("true.Filter(true) should be true")
		}
	})
}
