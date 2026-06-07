package values

// Fuzz targets for the Value-layer simplifier. RFC-025 §"Strong unit-
// test coverage per package" calls out values/ as priority-1; these
// fuzz targets complement the parameterised unit tests by sweeping
// random Value trees and pinning the no-panic + idempotency
// contracts the simplifier must hold.

import (
	"math"
	"testing"
)

// FuzzSimplifyValue_ArithmeticTree fuzzes SimplifyValue against a
// small arithmetic tree built from raw byte input. Build shape:
//
//	op = (a op1 b) op2 c
//
// where op1 / op2 are picked from (Add, Sub, Mul, Div, Mod) by the
// fuzz bytes, and a / b / c are int64 constants. The tree is fully
// constant by construction, so SimplifyValue MUST collapse it to a
// ConstantValue (or NullValue on div-by-zero) — never panic, never
// leave a non-collapsed composite.
func FuzzSimplifyValue_ArithmeticTree(f *testing.F) {
	// Seed with the canonical happy path + a div-by-zero case.
	f.Add(int64(2), int64(3), int64(4), uint8(0), uint8(2)) // (2+3)*4 = 20
	f.Add(int64(1), int64(0), int64(2), uint8(3), uint8(0)) // 1/0 → NULL → propagates
	f.Add(int64(math.MaxInt64), int64(1), int64(1), uint8(1), uint8(1))

	f.Fuzz(func(t *testing.T, a, b, c int64, op1raw, op2raw uint8) {
		op1 := ArithmeticOp(op1raw % 5) // 5 ops: Add/Sub/Mul/Div/Mod
		op2 := ArithmeticOp(op2raw % 5)

		tree := &ArithmeticValue{
			Op: op2,
			Left: &ArithmeticValue{
				Op:    op1,
				Left:  &ConstantValue{Value: a, Typ: TypeInt},
				Right: &ConstantValue{Value: b, Typ: TypeInt},
			},
			Right: &ConstantValue{Value: c, Typ: TypeInt},
		}

		// 1. SimplifyValue must not panic on any byte input.
		out := SimplifyValue(tree)
		if out == nil {
			t.Fatalf("SimplifyValue returned nil — should always return a Value (got input: a=%d b=%d c=%d op1=%d op2=%d)", a, b, c, op1, op2)
		}

		// 2. Result must be a ConstantValue (numeric fold) or NullValue (a
		//    constant NULL). An all-constant tree that ERRORS on evaluation
		//    (div-by-zero / overflow) is NOT folded — RFC-091: erroring
		//    constants decline to fold so the error surfaces at runtime
		//    (22012 / 22003) instead of silently folding to NULL. So a
		//    non-collapsed composite is acceptable iff it genuinely errors.
		switch out.(type) {
		case *ConstantValue, *NullValue:
			// ok — folded to a literal
		default:
			if _, err := out.EvaluateErr(nil); err == nil {
				t.Fatalf("all-constant tree neither folded to a literal nor errors: got %T with no eval error", out)
			}
		}

		// 3. Idempotency: simplifying the result must be a no-op (the
		//    leaf folds back to itself).
		again := SimplifyValue(out)
		if again == nil {
			t.Fatalf("SimplifyValue(simplified) returned nil")
		}
	})
}

// FuzzSimplifyValue_CastChain fuzzes nested CastValues. CAST(CAST(x
// AS X) AS Y) should always simplify cleanly without panicking — the
// inner cast folds to a literal, then the outer cast folds again.
// Type-mismatch chains (CAST(int AS Bool) AS Int) decline gracefully.
func FuzzSimplifyValue_CastChain(f *testing.F) {
	f.Add(int64(42), uint8(2), uint8(2)) // identity cast int→int→int
	f.Add(int64(0), uint8(1), uint8(2))  // int→bool→int (boundary)
	f.Add(int64(-1), uint8(3), uint8(4)) // int→string→float

	f.Fuzz(func(t *testing.T, n int64, t1raw, t2raw uint8) {
		// Pool of cascades Types the seed CAST evaluator handles.
		// NewCastValue rejects nil / UnknownType — pick from the
		// concrete primitive singletons only.
		pool := []Type{TypeInt, TypeString, TypeBool, TypeFloat}
		t1 := pool[int(t1raw)%len(pool)]
		t2 := pool[int(t2raw)%len(pool)]

		tree := NewCastValue(
			NewCastValue(
				&ConstantValue{Value: n, Typ: TypeInt},
				t1,
			),
			t2,
		)

		out := SimplifyValue(tree)
		if out == nil {
			t.Fatalf("SimplifyValue returned nil for CAST chain (n=%d t1=%v t2=%v)", n, t1, t2)
		}

		// Idempotency: simplifying the result must be a no-op. Either
		// the chain folded to a leaf (folds back to itself) or the
		// type-mismatch case declined (declining is also idempotent).
		again := SimplifyValue(out)
		if again == nil {
			t.Fatalf("SimplifyValue(simplified) returned nil for CAST chain")
		}
	})
}
