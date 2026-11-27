package values

import (
	"math"
	"testing"
)

// Swallow-axis tests (the RFC-087 gate). The plan-time constant-fold
// paths must DECLINE to fold on a data-dependent runtime error — leaving
// the node for runtime evaluation — rather than crashing or surfacing the
// error from the planner. These mirror `WHERE 5 = 'abc'`: a fully-constant
// sub-tree whose Evaluate raises a typed error must be swallowed, not
// propagated, at plan time. The complementary propagate edges (the error
// reaching a per-row Evaluate) are pinned in scalar_functions_errors_test.go
// and values_java_inspired_test.go; the programmer-invariant panic that must
// still surface is pinned by TestEvaluateConstant_ProgrammerInvariantPanicSurfaces.

// TestEvaluateConstant_TypeMismatch_DeclinesToFold pins the EvaluateConstant
// swallow path: `5 + true` is a constant tree (IsConstantValue == true)
// whose Evaluate raises a *ScalarTypeMismatchError. EvaluateConstant must
// return (nil, false) — decline — not crash or surface the error.
// (The old `5 + 'abc'` fixture stopped erroring when the Java ADD string
// family landed — number + string CONCATENATES per ArithmeticValue.java
// ADD_IS, so it now folds to "5abc".)
func TestEvaluateConstant_TypeMismatch_DeclinesToFold(t *testing.T) {
	t.Parallel()
	v := &ArithmeticValue{
		Op:    OpAdd,
		Left:  &ConstantValue{Value: int64(5), Typ: NullableLong},
		Right: &ConstantValue{Value: true, Typ: TypeBool},
	}
	if !IsConstantValue(v) {
		t.Fatalf("precondition: 5 + true should be a constant tree so the fold path is exercised")
	}
	out, ok := EvaluateConstant(v)
	if ok || out != nil {
		t.Fatalf("EvaluateConstant(5 + true): got (%v, %v), want (nil, false) — decline-to-fold", out, ok)
	}
}

// TestTryCastConstant_TypeMismatch_DeclinesToFold pins the tryCastConstant
// swallow path: CAST(NaN AS BIGINT) raises *InvalidCastError on the error
// channel. tryCastConstant must return nil (decline) — not crash or surface
// the error.
func TestTryCastConstant_TypeMismatch_DeclinesToFold(t *testing.T) {
	t.Parallel()
	out := tryCastConstant(&ConstantValue{Value: math.NaN(), Typ: NullableDouble}, NullableLong)
	if out != nil {
		t.Fatalf("tryCastConstant(NaN→BIGINT): got %v, want nil — decline-to-fold", out)
	}
}
