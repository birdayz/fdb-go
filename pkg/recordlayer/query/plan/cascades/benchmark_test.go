package cascades

import "testing"

// Micro-benchmarks for the Phase 4.0 cascades seed. These aren't
// a performance gate today — they're here so subsequent shifts can
// detect regressions as the real Value + predicate hierarchies
// land.

func BenchmarkConstantValue_Evaluate(b *testing.B) {
	v := &ConstantValue{Value: int64(42), Typ: TypeInt}
	for i := 0; i < b.N; i++ {
		_ = v.Evaluate(nil)
	}
}

func BenchmarkFieldValue_Evaluate(b *testing.B) {
	v := &FieldValue{Field: "age", Typ: TypeInt}
	row := map[string]any{"age": int64(30)}
	for i := 0; i < b.N; i++ {
		_ = v.Evaluate(row)
	}
}

func BenchmarkArithmeticValue_Evaluate(b *testing.B) {
	// (a + b) * (c - d)
	v := &ArithmeticValue{
		Op: OpMul,
		Left: &ArithmeticValue{
			Op:    OpAdd,
			Left:  &FieldValue{Field: "a", Typ: TypeInt},
			Right: &FieldValue{Field: "b", Typ: TypeInt},
		},
		Right: &ArithmeticValue{
			Op:    OpSub,
			Left:  &FieldValue{Field: "c", Typ: TypeInt},
			Right: &FieldValue{Field: "d", Typ: TypeInt},
		},
	}
	row := map[string]any{"a": int64(3), "b": int64(4), "c": int64(10), "d": int64(5)}
	for i := 0; i < b.N; i++ {
		_ = v.Evaluate(row)
	}
}

func BenchmarkComparisonPredicate_Eval(b *testing.B) {
	pred := NewComparisonPredicate(
		&FieldValue{Field: "age", Typ: TypeInt},
		Comparison{Type: ComparisonGreaterThanEq, Operand: int64(18)},
	)
	row := map[string]any{"age": int64(30)}
	for i := 0; i < b.N; i++ {
		_ = pred.Eval(row)
	}
}

func BenchmarkKleeneAnd_Eval(b *testing.B) {
	// (age >= 18) AND (rank < 5) AND (score > 50)
	tree := NewAnd(
		NewComparisonPredicate(&FieldValue{Field: "age", Typ: TypeInt},
			Comparison{Type: ComparisonGreaterThanEq, Operand: int64(18)}),
		NewComparisonPredicate(&FieldValue{Field: "rank", Typ: TypeInt},
			Comparison{Type: ComparisonLessThan, Operand: int64(5)}),
		NewComparisonPredicate(&FieldValue{Field: "score", Typ: TypeInt},
			Comparison{Type: ComparisonGreaterThan, Operand: int64(50)}),
	)
	row := map[string]any{"age": int64(30), "rank": int64(3), "score": int64(80)}
	for i := 0; i < b.N; i++ {
		_ = tree.Eval(row)
	}
}

func BenchmarkArithmeticMatcher_BindMatches(b *testing.B) {
	// Allocations matter here — each successful Bind copies the
	// PlannerBindings map. ReportAllocs surfaces the alloc count in
	// default `go test -bench` output without requiring -benchmem,
	// which matters for the stated regression-detection goal.
	b.ReportAllocs()
	// Match `ArithmeticValue(Add, ConstantValue, FieldValue)`.
	lhs := NewConstantMatcher()
	rhs := NewFieldMatcher()
	matcher := &ArithmeticMatcher{Op: OpAdd, Left: lhs, Right: rhs}
	expr := &ArithmeticValue{
		Op:    OpAdd,
		Left:  &ConstantValue{Value: int64(5), Typ: TypeInt},
		Right: &FieldValue{Field: "x", Typ: TypeInt},
	}
	outer := NewBindings()
	for i := 0; i < b.N; i++ {
		_ = matcher.BindMatches(outer, expr)
	}
}

func BenchmarkAllOf_BindMatches(b *testing.B) {
	b.ReportAllocs()
	// AllOf(ConstantMatcher, AnyValue) against a ConstantValue.
	pattern := NewAllOf("ConstantValue", NewConstantMatcher(), NewAnyValue())
	cv := &ConstantValue{Value: int64(7), Typ: TypeInt}
	outer := NewBindings()
	for i := 0; i < b.N; i++ {
		_ = pattern.BindMatches(outer, cv)
	}
}

// Fixed-point Simplify driver over a tree that exercises every rule
// DefaultSimplifyRules ships (flatten + constant folds + dedup). Same
// shape as TestSimplify_FullPipeline so regressions show up against a
// known capstone.
func BenchmarkSimplify_FullPipeline(b *testing.B) {
	b.ReportAllocs()
	agePred := NewComparisonPredicate(
		&FieldValue{Field: "age", Typ: TypeInt},
		Comparison{Type: ComparisonGreaterThanEq, Operand: int64(18)},
	)
	// Build fresh each iter — Simplify sees a pristine tree, not a
	// memoised folded one.
	rules := DefaultSimplifyRules()
	for i := 0; i < b.N; i++ {
		pred := NewAnd(
			NewAnd(
				NewComparisonPredicate(
					&ConstantValue{Value: int64(5), Typ: TypeInt},
					Comparison{Type: ComparisonEquals, Operand: int64(5)},
				),
				NewNot(NewNot(NewConstantPredicate(TriTrue))),
			),
			agePred,
			agePred,
			NewConstantPredicate(TriTrue),
		)
		_ = Simplify(pred, rules)
	}
}

// Opaque-input baseline: the driver fires through every rule but
// nothing yields. Measures the pure-dispatch overhead the planner
// pays per predicate that can't be folded.
func BenchmarkSimplify_NoOp(b *testing.B) {
	b.ReportAllocs()
	pred := NewComparisonPredicate(
		&FieldValue{Field: "age", Typ: TypeInt},
		Comparison{Type: ComparisonGreaterThanEq, Operand: int64(18)},
	)
	rules := DefaultSimplifyRules()
	for i := 0; i < b.N; i++ {
		_ = Simplify(pred, rules)
	}
}
