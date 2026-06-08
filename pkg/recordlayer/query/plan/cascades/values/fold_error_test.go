package values

import (
	"math"
	"testing"
)

// TestEvaluateConstant_DivByZeroDeclinesToFold pins RFC-091: a constant 1/0 must
// NOT fold. Previously it folded to NULL (EvaluateConstant returned ok=true,
// out=nil) — a silent plan-time divergence from the runtime path, which raises
// 22012. Declining to fold leaves the expression in place so the error surfaces at
// execution with the correct SQLSTATE.
func TestEvaluateConstant_DivByZeroDeclinesToFold(t *testing.T) {
	t.Parallel()
	div := &ArithmeticValue{
		Op:    OpDiv,
		Left:  &ConstantValue{Value: int64(1), Typ: TypeInt},
		Right: &ConstantValue{Value: int64(0), Typ: TypeInt},
	}
	if got, ok := EvaluateConstant(div); ok {
		t.Fatalf("constant 1/0 must NOT fold (must raise 22012 at runtime); folded to %v", got)
	}
	if got, ok := DefaultFolder().Fold(div); ok {
		t.Fatalf("DefaultFolder must decline 1/0; folded to %v", got)
	}
}

// TestEvaluateConstant_OverflowDeclinesToFold pins that a constant arithmetic
// overflow declines to fold (was folded to NULL pre-RFC-091).
func TestEvaluateConstant_OverflowDeclinesToFold(t *testing.T) {
	t.Parallel()
	add := &ArithmeticValue{
		Op:    OpAdd,
		Left:  &ConstantValue{Value: int64(math.MaxInt64), Typ: TypeInt},
		Right: &ConstantValue{Value: int64(1), Typ: TypeInt},
	}
	if got, ok := EvaluateConstant(add); ok {
		t.Fatalf("constant overflow must NOT fold; folded to %v", got)
	}
}

// TestEvaluateConstant_PlainConstantStillFolds guards the happy path: a constant
// that evaluates cleanly still folds (the fix only declines on error).
func TestEvaluateConstant_PlainConstantStillFolds(t *testing.T) {
	t.Parallel()
	v := &ArithmeticValue{
		Op:    OpAdd,
		Left:  &ConstantValue{Value: int64(2), Typ: TypeInt},
		Right: &ConstantValue{Value: int64(3), Typ: TypeInt},
	}
	got, ok := EvaluateConstant(v)
	if !ok || got != int64(5) {
		t.Fatalf("clean constant must still fold to 5; got (%v, %v)", got, ok)
	}
}
