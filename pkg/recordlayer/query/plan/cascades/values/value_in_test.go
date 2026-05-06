package values

import "testing"

func TestInOpValue_Evaluate_HitFirst(t *testing.T) {
	t.Parallel()
	v := NewInOpValue(
		LiteralValue(int64(2)),
		LiteralValue([]any{int64(1), int64(2), int64(3)}),
	)
	if got := v.Evaluate(nil); got != true {
		t.Fatalf("2 IN (1,2,3) = %v, want true", got)
	}
}

func TestInOpValue_Evaluate_Miss(t *testing.T) {
	t.Parallel()
	v := NewInOpValue(
		LiteralValue(int64(99)),
		LiteralValue([]any{int64(1), int64(2), int64(3)}),
	)
	if got := v.Evaluate(nil); got != false {
		t.Fatalf("99 IN (1,2,3) = %v, want false", got)
	}
}

func TestInOpValue_Evaluate_NullProbe(t *testing.T) {
	t.Parallel()
	v := NewInOpValue(
		LiteralValue(nil),
		LiteralValue([]any{int64(1), int64(2)}),
	)
	if got := v.Evaluate(nil); got != nil {
		t.Fatalf("NULL IN (1,2) = %v, want nil (UNKNOWN)", got)
	}
}

func TestInOpValue_Evaluate_NullInListMiss(t *testing.T) {
	t.Parallel()
	// 99 IN (1, NULL, 3) — probe doesn't match any non-NULL; NULL in
	// list propagates UNKNOWN.
	v := NewInOpValue(
		LiteralValue(int64(99)),
		LiteralValue([]any{int64(1), nil, int64(3)}),
	)
	if got := v.Evaluate(nil); got != nil {
		t.Fatalf("99 IN (1,NULL,3) = %v, want nil (UNKNOWN)", got)
	}
}

func TestInOpValue_Evaluate_NullInListHit(t *testing.T) {
	t.Parallel()
	// 1 IN (1, NULL, 3) — probe matches 1; NULL doesn't change result.
	v := NewInOpValue(
		LiteralValue(int64(1)),
		LiteralValue([]any{int64(1), nil, int64(3)}),
	)
	if got := v.Evaluate(nil); got != true {
		t.Fatalf("1 IN (1,NULL,3) = %v, want true", got)
	}
}

func TestInOpValue_Evaluate_EmptyListIsFalse(t *testing.T) {
	t.Parallel()
	v := NewInOpValue(
		LiteralValue(int64(1)),
		LiteralValue([]any{}),
	)
	if got := v.Evaluate(nil); got != false {
		t.Fatalf("1 IN () = %v, want false", got)
	}
}

func TestInOpValue_Type_IsNullableBoolean(t *testing.T) {
	t.Parallel()
	v := NewInOpValue(LiteralValue(int64(1)), LiteralValue([]any{int64(1)}))
	if v.Type() != NullableBoolean {
		t.Fatalf("Type=%v, want NullableBoolean", v.Type())
	}
}

func TestInOpValue_Children_BothOperands(t *testing.T) {
	t.Parallel()
	probe := LiteralValue(int64(1))
	list := LiteralValue([]any{int64(1)})
	v := NewInOpValue(probe, list)
	cs := v.Children()
	if len(cs) != 2 || cs[0] != probe || cs[1] != list {
		t.Fatalf("Children=%v, want [probe, list]", cs)
	}
}

func TestInOpValue_Children_NilOperandsDropped(t *testing.T) {
	t.Parallel()
	v := NewInOpValue(LiteralValue(int64(1)), nil)
	if got := len(v.Children()); got != 1 {
		t.Fatalf("Children with nil list returned %d, want 1", got)
	}
	v2 := NewInOpValue(nil, nil)
	if got := len(v2.Children()); got != 0 {
		t.Fatalf("Children with both nil returned %d, want 0", got)
	}
}

func TestInOpValue_Evaluate_ListNotSliceReturnsNil(t *testing.T) {
	t.Parallel()
	// List Value evaluates to a non-slice — type-degraded UNKNOWN.
	v := NewInOpValue(
		LiteralValue(int64(1)),
		LiteralValue("not a slice"),
	)
	if got := v.Evaluate(nil); got != nil {
		t.Fatalf("Evaluate with non-slice list = %v, want nil", got)
	}
}
