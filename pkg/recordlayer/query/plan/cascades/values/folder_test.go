package values

import "testing"

// TestDefaultFolder_NilSafe pins the boundary: nil input doesn't
// panic, returns (nil, false). Callers depend on this so they don't
// need to nil-guard before invoking Fold.
func TestDefaultFolder_NilSafe(t *testing.T) {
	t.Parallel()
	f := DefaultFolder()
	got, ok := f.Fold(nil)
	if got != nil || ok {
		t.Fatalf("Fold(nil): got (%v, %v), want (nil, false)", got, ok)
	}
}

// TestDefaultFolder_ConstantFolds_FullyComposed pins that the
// Default folder composes SimplifyValue + EvaluateConstant: a
// constant arithmetic tree folds to its computed value.
func TestDefaultFolder_ConstantFolds_FullyComposed(t *testing.T) {
	t.Parallel()
	f := DefaultFolder()
	v := &ArithmeticValue{
		Op: OpAdd,
		Left: &ArithmeticValue{
			Op:    OpMul,
			Left:  &ConstantValue{Value: int64(2), Typ: TypeInt},
			Right: &ConstantValue{Value: int64(3), Typ: TypeInt},
		},
		Right: &ConstantValue{Value: int64(1), Typ: TypeInt},
	}
	got, ok := f.Fold(v)
	if !ok {
		t.Fatalf("expected fold ok=true")
	}
	if got != int64(7) {
		t.Fatalf("expected 7, got %v", got)
	}
}

// TestDefaultFolder_FieldRefDeclines pins that a Value with a
// FieldValue leaf declines folding — IsConstantValue says no.
func TestDefaultFolder_FieldRefDeclines(t *testing.T) {
	t.Parallel()
	f := DefaultFolder()
	v := &ArithmeticValue{
		Op:    OpAdd,
		Left:  &FieldValue{Field: "x", Typ: TypeInt},
		Right: &ConstantValue{Value: int64(1), Typ: TypeInt},
	}
	if got, ok := f.Fold(v); ok {
		t.Fatalf("expected decline, got (%v, %v)", got, ok)
	}
}

// TestDefaultFolder_ParameterValueDeclines pins that a parameter-
// bound Value declines (binding happens at runtime, not plan time).
func TestDefaultFolder_ParameterValueDeclines(t *testing.T) {
	t.Parallel()
	f := DefaultFolder()
	v := &ArithmeticValue{
		Op:    OpMul,
		Left:  NewParameterValue(1),
		Right: &ConstantValue{Value: int64(2), Typ: TypeInt},
	}
	if got, ok := f.Fold(v); ok {
		t.Fatalf("expected decline for ParameterValue, got (%v, %v)", got, ok)
	}
}

// TestDefaultFolder_PartialFoldComposesViaSimplify pins the
// integration with SimplifyValue: an Arithmetic node `name + (1+2)`
// simplifies to `name + 3` — the inner constant node folds even
// though the outer node remains non-constant. The Folder's contract
// only returns ok=true on FULLY-folded results, but verifying that
// SimplifyValue ran requires inspecting the simplified tree directly.
func TestDefaultFolder_PartialFoldDoesNotReturnOk(t *testing.T) {
	t.Parallel()
	f := DefaultFolder()
	v := &ArithmeticValue{
		Op:   OpAdd,
		Left: &FieldValue{Field: "name", Typ: TypeInt},
		Right: &ArithmeticValue{
			Op:    OpAdd,
			Left:  &ConstantValue{Value: int64(1), Typ: TypeInt},
			Right: &ConstantValue{Value: int64(2), Typ: TypeInt},
		},
	}
	if got, ok := f.Fold(v); ok {
		t.Fatalf("partial fold should not be foldable, got (%v, %v)", got, ok)
	}
}
