package values

import "testing"

func TestEvaluatesToValue_IsTrue(t *testing.T) {
	t.Parallel()
	yes := NewEvaluatesToValue(LiteralValue(true), EvaluatesToTrue)
	if got := mustEvaluate(yes, nil); got != true {
		t.Fatalf("true IS TRUE = %v, want true", got)
	}
	no := NewEvaluatesToValue(LiteralValue(false), EvaluatesToTrue)
	if got := mustEvaluate(no, nil); got != false {
		t.Fatalf("false IS TRUE = %v, want false", got)
	}
}

func TestEvaluatesToValue_IsFalse(t *testing.T) {
	t.Parallel()
	yes := NewEvaluatesToValue(LiteralValue(false), EvaluatesToFalse)
	if got := mustEvaluate(yes, nil); got != true {
		t.Fatalf("false IS FALSE = %v, want true", got)
	}
}

func TestEvaluatesToValue_IsNull(t *testing.T) {
	t.Parallel()
	yes := NewEvaluatesToValue(LiteralValue(nil), EvaluatesToNull)
	if got := mustEvaluate(yes, nil); got != true {
		t.Fatalf("NULL IS NULL = %v, want true", got)
	}
	no := NewEvaluatesToValue(LiteralValue(int64(1)), EvaluatesToNull)
	if got := mustEvaluate(no, nil); got != false {
		t.Fatalf("1 IS NULL = %v, want false", got)
	}
}

func TestEvaluatesToValue_IsNotNull(t *testing.T) {
	t.Parallel()
	yes := NewEvaluatesToValue(LiteralValue(int64(1)), EvaluatesToNotNull)
	if got := mustEvaluate(yes, nil); got != true {
		t.Fatalf("1 IS NOT NULL = %v, want true", got)
	}
	no := NewEvaluatesToValue(LiteralValue(nil), EvaluatesToNotNull)
	if got := mustEvaluate(no, nil); got != false {
		t.Fatalf("NULL IS NOT NULL = %v, want false", got)
	}
}

func TestEvaluatesToValue_NonBoolIsTrueIsFalse(t *testing.T) {
	t.Parallel()
	v := NewEvaluatesToValue(LiteralValue(int64(1)), EvaluatesToTrue)
	if got := mustEvaluate(v, nil); got != false {
		t.Fatalf("1 IS TRUE = %v, want false (not a bool)", got)
	}
}

func TestEvaluatesToValue_NilChildIsNullPredicate(t *testing.T) {
	t.Parallel()
	v := NewEvaluatesToValue(nil, EvaluatesToNull)
	if got := mustEvaluate(v, nil); got != true {
		t.Fatalf("nil-child IS NULL = %v, want true", got)
	}
}

func TestEvaluatesToValue_TypeIsNotNullBoolean(t *testing.T) {
	t.Parallel()
	v := NewEvaluatesToValue(LiteralValue(true), EvaluatesToTrue)
	if !v.Type().Equals(NotNullBoolean) {
		t.Fatalf("Type=%v, want NotNullBoolean", v.Type())
	}
}

func TestEvaluatesToValue_Children(t *testing.T) {
	t.Parallel()
	c := LiteralValue(int64(1))
	v := NewEvaluatesToValue(c, EvaluatesToNull)
	cs := v.Children()
	if len(cs) != 1 || cs[0] != c {
		t.Fatalf("Children = %v, want [child]", cs)
	}
}

// TestEvaluatesToValue_SimplifyConstantFold verifies that
// SimplifyValue folds an all-constant EvaluatesToValue at plan time.
//   - true IS TRUE → ConstantValue(true)
//   - NULL IS NULL → ConstantValue(true)
//   - false IS NOT NULL → ConstantValue(true)
func TestEvaluatesToValue_SimplifyConstantFold(t *testing.T) {
	t.Parallel()
	cases := []struct {
		child any
		eval  EvaluatesTo
		want  any
	}{
		{true, EvaluatesToTrue, true},
		{false, EvaluatesToTrue, false},
		{nil, EvaluatesToTrue, false},
		{true, EvaluatesToFalse, false},
		{false, EvaluatesToFalse, true},
		{nil, EvaluatesToFalse, false},
		{true, EvaluatesToNull, false},
		{nil, EvaluatesToNull, true},
		{false, EvaluatesToNotNull, true},
		{nil, EvaluatesToNotNull, false},
	}
	for _, c := range cases {
		v := NewEvaluatesToValue(LiteralValue(c.child), c.eval)
		folded := SimplifyValue(v)
		got := mustEvaluate(folded, nil)
		if got != c.want {
			t.Errorf("EvaluatesTo(%v, %v): folded.Evaluate = %v, want %v",
				c.child, c.eval, got, c.want)
		}
	}
}

func TestEvaluatesToValue_SimplifyDoesNotFoldDivByZeroToNull(t *testing.T) {
	t.Parallel()
	// RFC-091: a child 1/0 must NOT fold to NULL at plan time, so `(1/0) IS NULL`
	// does NOT collapse to TRUE. The division raises division-by-zero (22012) at
	// runtime, matching Java — IS NULL does not swallow the error. (Previously the
	// constant fold swallowed 1/0→NULL and this returned TRUE — the bug Graefe
	// flagged.)
	div := &ArithmeticValue{
		Op:    OpDiv,
		Left:  &ConstantValue{Value: int64(1), Typ: NotNullLong},
		Right: &ConstantValue{Value: int64(0), Typ: NotNullLong},
	}
	v := NewEvaluatesToValue(div, EvaluatesToNull)
	folded := SimplifyValue(v)
	if _, err := folded.Evaluate(nil); err == nil {
		t.Fatal("(1/0) IS NULL must raise division-by-zero, not fold to TRUE")
	}
}
