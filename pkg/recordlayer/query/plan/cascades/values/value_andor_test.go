package values

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAndOrOp_String(t *testing.T) {
	t.Parallel()
	cases := map[AndOrOp]string{
		AndOrAnd:    "AND",
		AndOrOr:     "OR",
		AndOrOp(99): "INVALID",
	}
	for op, want := range cases {
		if got := op.String(); got != want {
			t.Errorf("AndOrOp(%d).String() = %q, want %q", op, got, want)
		}
	}
}

func TestAndOrValue_TypeBothNotNull(t *testing.T) {
	t.Parallel()
	// Both operands are NotNull booleans → result is NotNullBoolean.
	v := NewAndOrValue(AndOrAnd, NewBooleanValue(true), NewBooleanValue(false))
	if !v.Type().Equals(NotNullBoolean) {
		t.Fatalf("Type = %v, want NotNullBoolean (both NOT NULL operands)", v.Type())
	}
}

func TestAndOrValue_TypeNullableOperand(t *testing.T) {
	t.Parallel()
	// LiteralValue(nil) has nullable type → result is NullableBoolean.
	v := NewAndOrValue(AndOrAnd, NewBooleanValue(true), LiteralValue(nil))
	if !v.Type().Equals(NullableBoolean) {
		t.Fatalf("Type = %v, want NullableBoolean (one nullable operand)", v.Type())
	}
}

func TestAndOrValue_TypeNilOperandFallsBackToNullable(t *testing.T) {
	t.Parallel()
	v := NewAndOrValue(AndOrAnd, NewBooleanValue(true), nil)
	if !v.Type().Equals(NullableBoolean) {
		t.Fatalf("Type = %v, want NullableBoolean (nil operand fallback)", v.Type())
	}
}

func TestAndOrValue_Name(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		op   AndOrOp
		name string
	}{
		{AndOrAnd, "AND"},
		{AndOrOr, "OR"},
	} {
		v := NewAndOrValue(c.op, nil, nil)
		if got := v.Name(); got != c.name {
			t.Errorf("Op=%v Name = %q, want %q", c.op, got, c.name)
		}
	}
}

func TestAndOrValue_Children(t *testing.T) {
	t.Parallel()
	left := NewBooleanValue(true)
	right := NewBooleanValue(false)
	v := NewAndOrValue(AndOrAnd, left, right)
	cs := v.Children()
	if len(cs) != 2 || cs[0] != left || cs[1] != right {
		t.Fatalf("Children = %v, want [left, right]", cs)
	}
}

// AND truth table (per Kleene 3VL).
func TestAndOrValue_AndTruthTable(t *testing.T) {
	t.Parallel()
	type row struct {
		left, right any
		want        any
	}
	cases := []row{
		{true, true, true},
		{true, false, false},
		{true, nil, nil},
		{false, true, false},
		{false, false, false},
		{false, nil, false}, // FALSE AND NULL = FALSE
		{nil, true, nil},
		{nil, false, false}, // NULL AND FALSE = FALSE
		{nil, nil, nil},
	}
	for _, c := range cases {
		v := NewAndOrValue(AndOrAnd, LiteralValue(c.left), LiteralValue(c.right))
		got, errEv0 := v.Evaluate(nil)
		require.NoError(t, errEv0)
		if got != c.want {
			t.Errorf("AND(%v, %v) = %v, want %v", c.left, c.right, got, c.want)
		}
	}
}

// OR truth table (per Kleene 3VL).
func TestAndOrValue_OrTruthTable(t *testing.T) {
	t.Parallel()
	type row struct {
		left, right any
		want        any
	}
	cases := []row{
		{true, true, true},
		{true, false, true},
		{true, nil, true}, // TRUE OR NULL = TRUE
		{false, true, true},
		{false, false, false},
		{false, nil, nil},
		{nil, true, true}, // NULL OR TRUE = TRUE
		{nil, false, nil},
		{nil, nil, nil},
	}
	for _, c := range cases {
		v := NewAndOrValue(AndOrOr, LiteralValue(c.left), LiteralValue(c.right))
		got, errEv0 := v.Evaluate(nil)
		require.NoError(t, errEv0)
		if got != c.want {
			t.Errorf("OR(%v, %v) = %v, want %v", c.left, c.right, got, c.want)
		}
	}
}

func TestAndOrValue_NonBoolReturnsNil(t *testing.T) {
	t.Parallel()
	v := NewAndOrValue(AndOrAnd, LiteralValue("not-a-bool"), LiteralValue(true))
	got, errEv0 := v.Evaluate(nil)
	require.NoError(t, errEv0)
	if got != nil {
		t.Fatalf("AND(string, true) = %v, want nil (type-degraded)", got)
	}
}

func TestAndOrValue_NilOperandReturnsNil(t *testing.T) {
	t.Parallel()
	v := NewAndOrValue(AndOrAnd, nil, LiteralValue(true))
	got, errEv0 := v.Evaluate(nil)
	require.NoError(t, errEv0)
	if got != nil {
		t.Fatalf("AND(nil, true) = %v, want nil", got)
	}
}

func TestAndOrValue_ShortCircuitAndFalse(t *testing.T) {
	t.Parallel()
	// Right operand must NOT be evaluated when left=FALSE.
	count := 0
	rightCounter := &counterValue{onEvaluate: func() { count++ }, val: true}
	v := NewAndOrValue(AndOrAnd, NewBooleanValue(false), rightCounter)
	_, errEv0 := v.Evaluate(nil)
	require.NoError(t, errEv0)
	if count != 0 {
		t.Fatalf("Right operand evaluated %d times, want 0 (FALSE AND ? short-circuit)", count)
	}
}

func TestAndOrValue_ShortCircuitOrTrue(t *testing.T) {
	t.Parallel()
	// Right operand must NOT be evaluated when left=TRUE.
	count := 0
	rightCounter := &counterValue{onEvaluate: func() { count++ }, val: false}
	v := NewAndOrValue(AndOrOr, NewBooleanValue(true), rightCounter)
	_, errEv0 := v.Evaluate(nil)
	require.NoError(t, errEv0)
	if count != 0 {
		t.Fatalf("Right operand evaluated %d times, want 0 (TRUE OR ? short-circuit)", count)
	}
}

func TestAndOrValue_WithChildren(t *testing.T) {
	t.Parallel()
	original := NewAndOrValue(AndOrAnd, NewBooleanValue(true), NewBooleanValue(false))
	rebuilt := original.WithChildren([]Value{NewBooleanValue(false), NewBooleanValue(true)})
	if rebuilt.Op != AndOrAnd {
		t.Fatalf("rebuilt.Op = %v, want AND (carried through)", rebuilt.Op)
	}
	got, errEv0 := rebuilt.Evaluate(nil)
	require.NoError(t, errEv0)
	if got != false {
		t.Fatalf("rebuilt.Evaluate = %v, want false", got)
	}
}

// TestAndOrValue_SimplifyConstantFold verifies that SimplifyValue
// folds an all-constant AndOrValue into a literal. With the
// simplifier dispatch added in this same shift, a constant
// AND/OR expression should reduce to a single ConstantValue.
func TestAndOrValue_SimplifyConstantFold(t *testing.T) {
	t.Parallel()
	// AND(true, false) = false → should fold to a ConstantValue(false).
	v := NewAndOrValue(AndOrAnd, NewBooleanValue(true), NewBooleanValue(false))
	folded := SimplifyValue(v)
	if folded == v {
		t.Fatalf("SimplifyValue did NOT fold all-constant AndOrValue (returned same pointer)")
	}
	got, errEv0 := folded.Evaluate(nil)
	require.NoError(t, errEv0)
	if got != false {
		t.Fatalf("folded.Evaluate = %v, want false", got)
	}
}

func TestAndOrValue_SimplifyChildFold(t *testing.T) {
	t.Parallel()
	// AND(NOT(false), true). The NOT(false) → ConstantValue(true) fold
	// rebuilds the AndOrValue with the folded child; the rebuilt tree
	// is itself all-constant so it then folds to the final result.
	innerNot := NewNotValue(NewBooleanValue(false))
	outer := NewAndOrValue(AndOrAnd, innerNot, NewBooleanValue(true))
	folded := SimplifyValue(outer)
	got, errEv0 := folded.Evaluate(nil)
	require.NoError(t, errEv0)
	if got != true {
		t.Fatalf("folded(NOT(false) AND true).Evaluate = %v, want true", got)
	}
}

func TestAndOrValue_SimplifyNoConstFoldKeepsTree(t *testing.T) {
	t.Parallel()
	// AND(field, true) — field is non-constant → tree shape preserved.
	field := &FieldValue{Field: "active", Typ: TypeBool}
	v := NewAndOrValue(AndOrAnd, field, NewBooleanValue(true))
	folded := SimplifyValue(v)
	// Returns same pointer (no children changed).
	if folded != v {
		t.Fatalf("SimplifyValue rewrote unchanged tree")
	}
}
