package values

import "testing"

func TestPickValue_PicksByIndex(t *testing.T) {
	t.Parallel()
	alts := []Value{
		LiteralValue(int64(10)),
		LiteralValue(int64(20)),
		LiteralValue(int64(30)),
	}
	v0 := NewPickValue(LiteralValue(int64(0)), alts, NotNullLong)
	if got := mustEvaluate(v0, nil); got != int64(10) {
		t.Fatalf("Pick(0) = %v, want 10", got)
	}
	v1 := NewPickValue(LiteralValue(int64(1)), alts, NotNullLong)
	if got := mustEvaluate(v1, nil); got != int64(20) {
		t.Fatalf("Pick(1) = %v, want 20", got)
	}
	v2 := NewPickValue(LiteralValue(int64(2)), alts, NotNullLong)
	if got := mustEvaluate(v2, nil); got != int64(30) {
		t.Fatalf("Pick(2) = %v, want 30", got)
	}
}

func TestPickValue_NullSelectorReturnsNil(t *testing.T) {
	t.Parallel()
	v := NewPickValue(LiteralValue(nil), []Value{LiteralValue(int64(10))}, NotNullLong)
	if got := mustEvaluate(v, nil); got != nil {
		t.Fatalf("Pick(NULL) = %v, want nil", got)
	}
}

func TestPickValue_NonIntegerSelectorReturnsNil(t *testing.T) {
	t.Parallel()
	v := NewPickValue(LiteralValue("not-int"), []Value{LiteralValue(int64(10))}, NotNullLong)
	if got := mustEvaluate(v, nil); got != nil {
		t.Fatalf("Pick(string) = %v, want nil", got)
	}
}

func TestPickValue_OutOfBoundsReturnsNil(t *testing.T) {
	t.Parallel()
	v := NewPickValue(LiteralValue(int64(99)), []Value{LiteralValue(int64(10))}, NotNullLong)
	if got := mustEvaluate(v, nil); got != nil {
		t.Fatalf("Pick(99) = %v, want nil (out of bounds)", got)
	}
	v2 := NewPickValue(LiteralValue(int64(-1)), []Value{LiteralValue(int64(10))}, NotNullLong)
	if got := mustEvaluate(v2, nil); got != nil {
		t.Fatalf("Pick(-1) = %v, want nil (negative)", got)
	}
}

func TestPickValue_TypePreserved(t *testing.T) {
	t.Parallel()
	v := NewPickValue(LiteralValue(int64(0)), []Value{LiteralValue("x")}, NotNullString)
	if !v.Type().Equals(NotNullString) {
		t.Fatalf("Type = %v, want NotNullString", v.Type())
	}
}

func TestPickValue_Children(t *testing.T) {
	t.Parallel()
	sel := LiteralValue(int64(0))
	a := LiteralValue(int64(10))
	b := LiteralValue(int64(20))
	v := NewPickValue(sel, []Value{a, b}, NotNullLong)
	cs := v.Children()
	if len(cs) != 3 || cs[0] != sel || cs[1] != a || cs[2] != b {
		t.Fatalf("Children = %v, want [sel, a, b]", cs)
	}
}
