package values

import "testing"

func TestCollateValue_Type(t *testing.T) {
	t.Parallel()
	v := NewCollateValue(LiteralValue("hello"), nil, nil)
	if !v.Type().Equals(NotNullBytes) {
		t.Fatalf("Type = %v, want NotNullBytes", v.Type())
	}
}

func TestCollateValue_Name(t *testing.T) {
	t.Parallel()
	v := NewCollateValue(LiteralValue("hello"), nil, nil)
	if got := v.Name(); got != "collate" {
		t.Fatalf("Name = %q, want collate", got)
	}
}

func TestCollateValue_ChildrenString(t *testing.T) {
	t.Parallel()
	s := LiteralValue("hello")
	v := NewCollateValue(s, nil, nil)
	cs := v.Children()
	if len(cs) != 1 || cs[0] != s {
		t.Fatalf("Children = %v, want [string]", cs)
	}
}

func TestCollateValue_ChildrenWithLocale(t *testing.T) {
	t.Parallel()
	s := LiteralValue("hello")
	l := LiteralValue("en_US")
	v := NewCollateValue(s, l, nil)
	cs := v.Children()
	if len(cs) != 2 || cs[0] != s || cs[1] != l {
		t.Fatalf("Children = %v, want [string, locale]", cs)
	}
}

func TestCollateValue_ChildrenWithStrength(t *testing.T) {
	t.Parallel()
	s := LiteralValue("hello")
	l := LiteralValue("en_US")
	st := LiteralValue("PRIMARY")
	v := NewCollateValue(s, l, st)
	cs := v.Children()
	if len(cs) != 3 || cs[0] != s || cs[1] != l || cs[2] != st {
		t.Fatalf("Children = %v, want [string, locale, strength]", cs)
	}
}

func TestCollateValue_EvaluateIsPlaceholder(t *testing.T) {
	t.Parallel()
	v := NewCollateValue(LiteralValue("hello"), nil, nil)
	if got := v.Evaluate(nil); got != nil {
		t.Fatalf("Evaluate = %v, want nil (placeholder)", got)
	}
}

func TestCollateValue_WithChildrenStringOnly(t *testing.T) {
	t.Parallel()
	original := NewCollateValue(LiteralValue("a"), LiteralValue("b"), LiteralValue("c"))
	rebuilt := original.WithChildren([]Value{LiteralValue("X")})
	if rebuilt.LocaleChild != nil || rebuilt.StrengthChild != nil {
		t.Fatalf("WithChildren(1) didn't drop optional children")
	}
}

func TestCollateValue_WithChildrenStringAndLocale(t *testing.T) {
	t.Parallel()
	original := NewCollateValue(LiteralValue("a"), nil, nil)
	rebuilt := original.WithChildren([]Value{
		LiteralValue("X"),
		LiteralValue("Y"),
	})
	if rebuilt.LocaleChild == nil {
		t.Fatalf("WithChildren(2) didn't set Locale")
	}
	if rebuilt.StrengthChild != nil {
		t.Fatalf("WithChildren(2) shouldn't set Strength")
	}
}

func TestCollateValue_WithChildrenAll(t *testing.T) {
	t.Parallel()
	original := NewCollateValue(LiteralValue("a"), nil, nil)
	rebuilt := original.WithChildren([]Value{
		LiteralValue("X"),
		LiteralValue("Y"),
		LiteralValue("Z"),
	})
	if rebuilt.LocaleChild == nil || rebuilt.StrengthChild == nil {
		t.Fatalf("WithChildren(3) didn't set both optional children")
	}
}

func TestCollateValue_WithChildrenEmptyReturnsSelf(t *testing.T) {
	t.Parallel()
	original := NewCollateValue(LiteralValue("a"), nil, nil)
	rebuilt := original.WithChildren(nil)
	if rebuilt != original {
		t.Fatalf("WithChildren(empty) should return self")
	}
}
