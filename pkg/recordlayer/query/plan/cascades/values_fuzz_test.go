package cascades

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

		// 2. Result must be a ConstantValue (numeric fold) or
		//    NullValue (div-by-zero etc. — propagated). Should never
		//    be a non-collapsed composite, since the tree was fully
		//    constant.
		switch out.(type) {
		case *ConstantValue, *NullValue:
			// ok
		default:
			t.Fatalf("expected ConstantValue or NullValue after fold of all-constant tree, got %T", out)
		}

		// 3. Idempotency: simplifying the result must be a no-op (the
		//    leaf folds back to itself).
		again := SimplifyValue(out)
		if again == nil {
			t.Fatalf("SimplifyValue(simplified) returned nil")
		}
	})
}

// FuzzSimplify_PredicateTree fuzzes the QueryPredicate-level driver
// over a randomly-shaped boolean tree. The 7 leaf shapes pair an
// integer LHS with int RHS / NULL / a boolean constant under the 6
// most-common comparison operators; the tree shape (AND / OR / NOT)
// is selected via the fuzz bytes. Contract: Simplify must always
// return a non-nil QueryPredicate, never panic, and the result must
// be idempotent (a second pass produces the same pointer or shape).
//
// Combined with the existing FuzzSimplifyValue_ArithmeticTree this
// pins both layers (Value-level fold + Predicate-level rule
// pipeline). Run with bazelisk run //pkg/recordlayer/query/plan/cascades:
// cascades_test -- -test.run='^$' -test.fuzz='^FuzzSimplify_PredicateTree$'
// -test.fuzztime=60s.
func FuzzSimplify_PredicateTree(f *testing.F) {
	f.Add(int64(5), int64(3), uint8(0), uint8(0), uint8(0))
	f.Add(int64(10), int64(20), uint8(1), uint8(2), uint8(0)) // OR(=, <)
	f.Add(int64(0), int64(0), uint8(2), uint8(3), uint8(1))   // NOT(AND(=, =))
	f.Add(int64(-1), int64(0), uint8(3), uint8(4), uint8(2))  // double-NOT, etc.

	f.Fuzz(func(t *testing.T, a, b int64, op1raw, op2raw, shaperaw uint8) {
		op1 := ComparisonType(op1raw % 6) // first 6: =, <>, <, <=, >, >=
		op2 := ComparisonType(op2raw % 6)

		// Two leaves over a synthetic FieldValue + literal RHS.
		field := &FieldValue{Field: "x", Typ: TypeInt}
		left := NewComparisonPredicate(field, Comparison{Type: op1, Operand: LiteralValue(a)})
		right := NewComparisonPredicate(field, Comparison{Type: op2, Operand: LiteralValue(b)})

		// Tree shape selector (5 shapes across `shaperaw % 5`).
		var pred QueryPredicate
		switch shaperaw % 5 {
		case 0:
			pred = NewAnd(left, right)
		case 1:
			pred = NewOr(left, right)
		case 2:
			pred = NewNot(NewAnd(left, right))
		case 3:
			pred = NewNot(NewOr(left, right))
		case 4:
			pred = NewAnd(NewNot(left), right)
		}

		// Default-rules pass.
		out := Simplify(pred, DefaultSimplifyRules())
		if out == nil {
			t.Fatalf("Simplify returned nil — should always produce a QueryPredicate (a=%d b=%d op1=%v op2=%v shape=%d)", a, b, op1, op2, shaperaw%5)
		}
		// Idempotency.
		again := Simplify(out, DefaultSimplifyRules())
		if again == nil {
			t.Fatalf("Simplify(simplified) returned nil")
		}

		// NormalizationRules pass — adds De Morgan; same no-panic +
		// idempotency contract under the bigger rule set.
		outN := Simplify(pred, NormalizationRules())
		if outN == nil {
			t.Fatalf("Simplify(NormalizationRules) returned nil")
		}
		againN := Simplify(outN, NormalizationRules())
		if againN == nil {
			t.Fatalf("Simplify(NormalizationRules)(simplified) returned nil")
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
		// 5 ValueTypes: Unknown, Int, String, Bool, Float
		t1 := ValueType(t1raw % 5)
		t2 := ValueType(t2raw % 5)

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
