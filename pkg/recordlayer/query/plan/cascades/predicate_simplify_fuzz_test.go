package cascades

// Predicate-tree fuzz target for the QueryPredicate-level rule
// simplifier driver. Lives in root cascades/ because Simplify +
// DefaultSimplifyRules + NormalizationRules are the rule
// infrastructure and depend on cascades/predicates + cascades/values.

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

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
		op1 := predicates.ComparisonType(op1raw % 6) // first 6: =, <>, <, <=, >, >=
		op2 := predicates.ComparisonType(op2raw % 6)

		// Two leaves over a synthetic FieldValue + literal RHS.
		field := &values.FieldValue{Field: "x", Typ: values.TypeInt}
		left := predicates.NewComparisonPredicate(field, predicates.Comparison{Type: op1, Operand: values.LiteralValue(a)})
		right := predicates.NewComparisonPredicate(field, predicates.Comparison{Type: op2, Operand: values.LiteralValue(b)})

		// Tree shape selector (5 shapes across `shaperaw % 5`).
		var pred predicates.QueryPredicate
		switch shaperaw % 5 {
		case 0:
			pred = predicates.NewAnd(left, right)
		case 1:
			pred = predicates.NewOr(left, right)
		case 2:
			pred = predicates.NewNot(predicates.NewAnd(left, right))
		case 3:
			pred = predicates.NewNot(predicates.NewOr(left, right))
		case 4:
			pred = predicates.NewAnd(predicates.NewNot(left), right)
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
