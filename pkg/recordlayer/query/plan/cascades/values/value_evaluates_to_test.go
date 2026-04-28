package values

import "testing"

func TestEvaluatesToValue_IsTrue(t *testing.T) {
	t.Parallel()
	yes := NewEvaluatesToValue(LiteralValue(true), EvaluatesToTrue)
	if got := yes.Evaluate(nil); got != true {
		t.Fatalf("true IS TRUE = %v, want true", got)
	}
	no := NewEvaluatesToValue(LiteralValue(false), EvaluatesToTrue)
	if got := no.Evaluate(nil); got != false {
		t.Fatalf("false IS TRUE = %v, want false", got)
	}
}

func TestEvaluatesToValue_IsFalse(t *testing.T) {
	t.Parallel()
	yes := NewEvaluatesToValue(LiteralValue(false), EvaluatesToFalse)
	if got := yes.Evaluate(nil); got != true {
		t.Fatalf("false IS FALSE = %v, want true", got)
	}
}

func TestEvaluatesToValue_IsNull(t *testing.T) {
	t.Parallel()
	yes := NewEvaluatesToValue(LiteralValue(nil), EvaluatesToNull)
	if got := yes.Evaluate(nil); got != true {
		t.Fatalf("NULL IS NULL = %v, want true", got)
	}
	no := NewEvaluatesToValue(LiteralValue(int64(1)), EvaluatesToNull)
	if got := no.Evaluate(nil); got != false {
		t.Fatalf("1 IS NULL = %v, want false", got)
	}
}

func TestEvaluatesToValue_IsNotNull(t *testing.T) {
	t.Parallel()
	yes := NewEvaluatesToValue(LiteralValue(int64(1)), EvaluatesToNotNull)
	if got := yes.Evaluate(nil); got != true {
		t.Fatalf("1 IS NOT NULL = %v, want true", got)
	}
	no := NewEvaluatesToValue(LiteralValue(nil), EvaluatesToNotNull)
	if got := no.Evaluate(nil); got != false {
		t.Fatalf("NULL IS NOT NULL = %v, want false", got)
	}
}

func TestEvaluatesToValue_NonBoolIsTrueIsFalse(t *testing.T) {
	t.Parallel()
	v := NewEvaluatesToValue(LiteralValue(int64(1)), EvaluatesToTrue)
	if got := v.Evaluate(nil); got != false {
		t.Fatalf("1 IS TRUE = %v, want false (not a bool)", got)
	}
}

func TestEvaluatesToValue_NilChildIsNullPredicate(t *testing.T) {
	t.Parallel()
	v := NewEvaluatesToValue(nil, EvaluatesToNull)
	if got := v.Evaluate(nil); got != true {
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
