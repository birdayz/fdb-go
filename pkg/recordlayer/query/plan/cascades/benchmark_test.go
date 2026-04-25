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
		Comparison{Type: ComparisonGreaterThanEq, Operand: LiteralValue(int64(18))},
	)
	row := map[string]any{"age": int64(30)}
	for i := 0; i < b.N; i++ {
		_ = pred.Eval(row)
	}
}

// Non-constant RHS exercises the second Operand.Evaluate(evalCtx)
// call ComparisonPredicate.Eval grew this shift. Pin the cost
// against the constant-RHS baseline so a future pessimisation
// (extra alloc, redundant nil-guard, etc.) shows up in CI bench.
//
// The predicate is `age = cutoff` evaluated against a row carrying
// both fields. Eval reads both LHS and RHS via map lookup before
// EvalAgainst's int64 promotion and comparison.
func BenchmarkComparisonPredicate_Eval_NonConstantRHS(b *testing.B) {
	pred := NewComparisonPredicate(
		&FieldValue{Field: "age", Typ: TypeInt},
		Comparison{Type: ComparisonEquals, Operand: &FieldValue{Field: "cutoff", Typ: TypeInt}},
	)
	row := map[string]any{"age": int64(18), "cutoff": int64(18)}
	for i := 0; i < b.N; i++ {
		_ = pred.Eval(row)
	}
}

func BenchmarkKleeneAnd_Eval(b *testing.B) {
	// (age >= 18) AND (rank < 5) AND (score > 50)
	tree := NewAnd(
		NewComparisonPredicate(&FieldValue{Field: "age", Typ: TypeInt},
			Comparison{Type: ComparisonGreaterThanEq, Operand: LiteralValue(int64(18))}),
		NewComparisonPredicate(&FieldValue{Field: "rank", Typ: TypeInt},
			Comparison{Type: ComparisonLessThan, Operand: LiteralValue(int64(5))}),
		NewComparisonPredicate(&FieldValue{Field: "score", Typ: TypeInt},
			Comparison{Type: ComparisonGreaterThan, Operand: LiteralValue(int64(50))}),
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
		Comparison{Type: ComparisonGreaterThanEq, Operand: LiteralValue(int64(18))},
	)
	// Build fresh each iter — Simplify sees a pristine tree, not a
	// memoised folded one.
	rules := DefaultSimplifyRules()
	for i := 0; i < b.N; i++ {
		pred := NewAnd(
			NewAnd(
				NewComparisonPredicate(
					&ConstantValue{Value: int64(5), Typ: TypeInt},
					Comparison{Type: ComparisonEquals, Operand: LiteralValue(int64(5))},
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

// Absorption workload: p AND (p OR q) OR r — sees the absorption
// rule fire plus dedup + constant-fold. Baseline for the 11-rule
// rule set vs 8-rule (absorption + NotComparisonRewrite added this
// shift post-compaction).
func BenchmarkSimplify_Absorption(b *testing.B) {
	b.ReportAllocs()
	p := NewComparisonPredicate(
		&FieldValue{Field: "a", Typ: TypeInt},
		Comparison{Type: ComparisonEquals, Operand: LiteralValue(int64(1))},
	)
	q := NewComparisonPredicate(
		&FieldValue{Field: "b", Typ: TypeInt},
		Comparison{Type: ComparisonEquals, Operand: LiteralValue(int64(2))},
	)
	rules := DefaultSimplifyRules()
	for i := 0; i < b.N; i++ {
		// Fresh per-iteration so we don't memoise.
		pred := NewAnd(p, NewOr(p, q))
		_ = Simplify(pred, rules)
	}
}

// BenchmarkListMatcher_BindMatches measures positional-pairing match
// cost — the new matcher introduced this shift. Three downstreams,
// successful match. ReportAllocs surfaces the per-position append +
// host-bind alloc counts.
func BenchmarkListMatcher_BindMatches(b *testing.B) {
	b.ReportAllocs()
	matcher := NewListMatcher(NewConstantMatcher(), NewFieldMatcher(), NewConstantMatcher())
	in := []any{
		&ConstantValue{Value: int64(1), Typ: TypeInt},
		&FieldValue{Field: "x", Typ: TypeInt},
		&ConstantValue{Value: int64(2), Typ: TypeInt},
	}
	outer := NewBindings()
	for i := 0; i < b.N; i++ {
		_ = matcher.BindMatches(outer, in)
	}
}

// BenchmarkAllElementsMatcher_BindMatches measures the per-element
// cost of the all-same-downstream matcher. 5 elements, all match.
func BenchmarkAllElementsMatcher_BindMatches(b *testing.B) {
	b.ReportAllocs()
	matcher := NewAllElementsMatcher(NewConstantMatcher())
	in := []any{
		&ConstantValue{Value: int64(1), Typ: TypeInt},
		&ConstantValue{Value: int64(2), Typ: TypeInt},
		&ConstantValue{Value: int64(3), Typ: TypeInt},
		&ConstantValue{Value: int64(4), Typ: TypeInt},
		&ConstantValue{Value: int64(5), Typ: TypeInt},
	}
	outer := NewBindings()
	for i := 0; i < b.N; i++ {
		_ = matcher.BindMatches(outer, in)
	}
}

// BenchmarkSimplify_DeMorgan exercises the NormalizationRules path:
// NOT(AND(p,q)) → OR(NOT p, NOT q) → OR(p<>, q<>) once
// NotComparisonRewriteRule fires. Establishes a baseline for the
// extra rule set's overhead vs DefaultSimplifyRules-only.
func BenchmarkSimplify_DeMorgan(b *testing.B) {
	b.ReportAllocs()
	a := &FieldValue{Field: "a", Typ: TypeInt}
	bb := &FieldValue{Field: "b", Typ: TypeInt}
	rules := NormalizationRules()
	for i := 0; i < b.N; i++ {
		// Fresh tree per iter — Simplify mutates via rebuild.
		pred := NewNot(NewAnd(
			NewComparisonPredicate(a, Comparison{Type: ComparisonEquals, Operand: LiteralValue(int64(1))}),
			NewComparisonPredicate(bb, Comparison{Type: ComparisonEquals, Operand: LiteralValue(int64(2))}),
		))
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
		Comparison{Type: ComparisonGreaterThanEq, Operand: LiteralValue(int64(18))},
	)
	rules := DefaultSimplifyRules()
	for i := 0; i < b.N; i++ {
		_ = Simplify(pred, rules)
	}
}
