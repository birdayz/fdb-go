package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// Micro-benchmarks for the cascades planner primitives. These aren't
// a performance gate — they exist so Value / predicate / planner
// regressions show up in `just bench` comparisons.
//
// Every benchmark here VALIDATES the work it times. A benchmark that
// discards the result records an excellent number for a regression that
// turns the operation into an instant failure — planning that errors out,
// a rule that stops matching, an exploration that never converges all look
// like speedups. The validation is deliberately split by where it can sit
// without distorting the measurement:
//
//   - Loop-invariant, deterministic operations (Value.Evaluate,
//     predicate Eval, matcher BindMatches over a fixed input) are validated
//     ONCE before b.ResetTimer(). The same input yields the same result on
//     every iteration, so one check proves the whole loop while the timed
//     region keeps doing exactly the measured work and nothing else. This
//     matters at the nanosecond scale these run at, where an in-loop
//     nil-compare would be a measurable fraction of the sample. (The two
//     Simplify benchmarks additionally hoist their per-iteration tree
//     construction into a `build` closure so the pre-loop check builds the
//     same tree the loop does; that adds one call per iteration to both the
//     before and after of any comparison, matching the shape the other
//     planner benchmarks in this file already use.)
//   - Per-iteration work (planner runs, memoization, GetBest) is validated
//     INSIDE the loop, because each iteration builds a fresh Reference and a
//     fresh single-use Planner, so no pre-loop check can speak for them. The
//     added cost is a handful of comparisons against microsecond-scale work,
//     and it is paid identically by every revision being compared, so it
//     cannot manufacture or mask a delta. b.StopTimer/StartTimer is
//     deliberately NOT used for this: the pair costs far more than the
//     comparisons it would exclude and perturbs b.N estimation.

// benchRow is the minimal values.OrdinalRow the Value/predicate benchmarks
// evaluate against. Production flows an ordinal-model row (structurally
// executor.PositionalRow); a name-keyed map[string]any is NOT a recognized
// eval context, so a FieldValue over one returns *UnboundEvalContextError
// immediately. Benchmarking that tail measures the error construction, not
// evaluation — which is precisely why these benchmarks assert their result.
//
// SCOPE, so nobody reads more into these numbers than they carry: this is a
// bare []any, NOT executor.PositionalRow. It satisfies the same interface but
// has a trivial Get, so these benchmarks measure Value/predicate evaluation
// and its interface dispatch — not the production row implementation's own
// lookup cost. That is the right boundary for a micro-benchmark of the Value
// tree (and cascades cannot import executor anyway; the interface exists here
// precisely to break that cycle). End-to-end row cost belongs to the executor
// and SQL-layer benchmarks.
type benchRow []any

func (r benchRow) Get(ordinal int) (any, bool) {
	if ordinal < 0 || ordinal >= len(r) {
		return nil, false
	}
	return r[ordinal], true
}

func BenchmarkConstantValue_Evaluate(b *testing.B) {
	v := &values.ConstantValue{Value: int64(42), Typ: values.NullableLong}
	if got, err := v.Evaluate(nil); err != nil || got != int64(42) {
		b.Fatalf("Evaluate = %v, err %v; want 42, nil", got, err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = v.Evaluate(nil)
	}
}

func BenchmarkFieldValue_Evaluate(b *testing.B) {
	v := values.NewFieldValueWithResolvedOrdinal("age", 0, values.NullableLong)
	row := benchRow{int64(30)}
	if got, err := v.Evaluate(row); err != nil || got != int64(30) {
		b.Fatalf("Evaluate = %v, err %v; want 30, nil", got, err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = v.Evaluate(row)
	}
}

func BenchmarkArithmeticValue_Evaluate(b *testing.B) {
	// (a + b) * (c - d)
	v := &values.ArithmeticValue{
		Op: values.OpMul,
		Left: &values.ArithmeticValue{
			Op:    values.OpAdd,
			Left:  values.NewFieldValueWithResolvedOrdinal("a", 0, values.NullableLong),
			Right: values.NewFieldValueWithResolvedOrdinal("b", 1, values.NullableLong),
		},
		Right: &values.ArithmeticValue{
			Op:    values.OpSub,
			Left:  values.NewFieldValueWithResolvedOrdinal("c", 2, values.NullableLong),
			Right: values.NewFieldValueWithResolvedOrdinal("d", 3, values.NullableLong),
		},
	}
	row := benchRow{int64(3), int64(4), int64(10), int64(5)}
	// (3+4) * (10-5)
	if got, err := v.Evaluate(row); err != nil || got != int64(35) {
		b.Fatalf("Evaluate = %v, err %v; want 35, nil", got, err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = v.Evaluate(row)
	}
}

func BenchmarkComparisonPredicate_Eval(b *testing.B) {
	pred := predicates.NewComparisonPredicate(
		values.NewFieldValueWithResolvedOrdinal("age", 0, values.NullableLong),
		predicates.Comparison{Type: predicates.ComparisonGreaterThanEq, Operand: values.LiteralValue(int64(18))},
	)
	row := benchRow{int64(30)}
	if got, err := pred.Eval(row); err != nil || got != predicates.TriTrue {
		b.Fatalf("Eval = %v, err %v; want TriTrue, nil", got, err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = pred.Eval(row)
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
	pred := predicates.NewComparisonPredicate(
		values.NewFieldValueWithResolvedOrdinal("age", 0, values.NullableLong),
		predicates.Comparison{Type: predicates.ComparisonEquals, Operand: values.NewFieldValueWithResolvedOrdinal("cutoff", 1, values.NullableLong)},
	)
	row := benchRow{int64(18), int64(18)}
	if got, err := pred.Eval(row); err != nil || got != predicates.TriTrue {
		b.Fatalf("Eval = %v, err %v; want TriTrue, nil", got, err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = pred.Eval(row)
	}
}

func BenchmarkKleeneAnd_Eval(b *testing.B) {
	// (age >= 18) AND (rank < 5) AND (score > 50)
	tree := predicates.NewAnd(
		predicates.NewComparisonPredicate(values.NewFieldValueWithResolvedOrdinal("age", 0, values.NullableLong),
			predicates.Comparison{Type: predicates.ComparisonGreaterThanEq, Operand: values.LiteralValue(int64(18))}),
		predicates.NewComparisonPredicate(values.NewFieldValueWithResolvedOrdinal("rank", 1, values.NullableLong),
			predicates.Comparison{Type: predicates.ComparisonLessThan, Operand: values.LiteralValue(int64(5))}),
		predicates.NewComparisonPredicate(values.NewFieldValueWithResolvedOrdinal("score", 2, values.NullableLong),
			predicates.Comparison{Type: predicates.ComparisonGreaterThan, Operand: values.LiteralValue(int64(50))}),
	)
	row := benchRow{int64(30), int64(3), int64(80)}
	if got, err := tree.Eval(row); err != nil || got != predicates.TriTrue {
		b.Fatalf("Eval = %v, err %v; want TriTrue, nil", got, err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = tree.Eval(row)
	}
}

func BenchmarkArithmeticMatcher_BindMatches(b *testing.B) {
	// Allocations matter here — each successful Bind copies the
	// PlannerBindings map. ReportAllocs surfaces the alloc count in
	// default `go test -bench` output without requiring -benchmem,
	// which matters for the stated regression-detection goal.
	b.ReportAllocs()
	// Match `ArithmeticValue(Add, ConstantValue, FieldValue)`.
	lhs := matching.NewConstantMatcher()
	rhs := matching.NewFieldMatcher()
	matcher := &matching.ArithmeticMatcher{Op: values.OpAdd, Left: lhs, Right: rhs}
	expr := &values.ArithmeticValue{
		Op:    values.OpAdd,
		Left:  &values.ConstantValue{Value: int64(5), Typ: values.NullableLong},
		Right: &values.FieldValue{Field: "x", Typ: values.NullableLong},
	}
	outer := matching.NewBindings()
	if got := matcher.BindMatches(outer, expr); len(got) == 0 {
		b.Fatal("ArithmeticMatcher did not match — the benchmark would time a rejection, not a bind")
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = matcher.BindMatches(outer, expr)
	}
}

func BenchmarkAllOf_BindMatches(b *testing.B) {
	b.ReportAllocs()
	// AllOf(ConstantMatcher, AnyValue) against a ConstantValue.
	pattern := matching.NewAllOf("ConstantValue", matching.NewConstantMatcher(), matching.NewAnyValue())
	cv := &values.ConstantValue{Value: int64(7), Typ: values.NullableLong}
	outer := matching.NewBindings()
	if got := pattern.BindMatches(outer, cv); len(got) == 0 {
		b.Fatal("AllOf did not match — the benchmark would time a rejection, not a bind")
	}
	b.ResetTimer()
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
	agePred := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "age", Typ: values.NullableLong},
		predicates.Comparison{Type: predicates.ComparisonGreaterThanEq, Operand: values.LiteralValue(int64(18))},
	)
	// Build fresh each iter — Simplify sees a pristine tree, not a
	// memoised folded one.
	build := func() predicates.QueryPredicate {
		return predicates.NewAnd(
			predicates.NewAnd(
				predicates.NewComparisonPredicate(
					&values.ConstantValue{Value: int64(5), Typ: values.NullableLong},
					predicates.Comparison{Type: predicates.ComparisonEquals, Operand: values.LiteralValue(int64(5))},
				),
				predicates.NewNot(predicates.NewNot(predicates.NewConstantPredicate(predicates.TriTrue))),
			),
			agePred,
			agePred,
			predicates.NewConstantPredicate(predicates.TriTrue),
		)
	}
	rules := DefaultSimplifyRules()
	// The whole tree must collapse to `agePred` (same expectation as
	// TestSimplify_FullPipeline). A rule set that stops firing would leave the
	// full tree standing and time a cheap no-op as if it were the pipeline.
	if got := Simplify(build(), rules); got != predicates.QueryPredicate(agePred) {
		b.Fatalf("Simplify = %T %s; want agePred", got, got.Explain())
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = Simplify(build(), rules)
	}
}

// Absorption workload: p AND (p OR q) OR r — sees the absorption
// rule fire plus dedup + constant-fold. Baseline for the 11-rule
// rule set vs 8-rule (absorption + NotComparisonRewrite added this
// shift post-compaction).
func BenchmarkSimplify_Absorption(b *testing.B) {
	b.ReportAllocs()
	p := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "a", Typ: values.NullableLong},
		predicates.Comparison{Type: predicates.ComparisonEquals, Operand: values.LiteralValue(int64(1))},
	)
	q := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "b", Typ: values.NullableLong},
		predicates.Comparison{Type: predicates.ComparisonEquals, Operand: values.LiteralValue(int64(2))},
	)
	rules := DefaultSimplifyRules()
	// Absorption must reduce `p AND (p OR q)` to `p`; if it stops firing the
	// benchmark would time the un-absorbed tree and read as a speedup.
	if got := Simplify(predicates.NewAnd(p, predicates.NewOr(p, q)), rules); got != predicates.QueryPredicate(p) {
		b.Fatalf("Simplify = %T %s; want p (absorption)", got, got.Explain())
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Fresh per-iteration so we don't memoise.
		pred := predicates.NewAnd(p, predicates.NewOr(p, q))
		_ = Simplify(pred, rules)
	}
}

// BenchmarkListMatcher_BindMatches measures positional-pairing match
// cost — the new matcher introduced this shift. Three downstreams,
// successful match. ReportAllocs surfaces the per-position append +
// host-bind alloc counts.
func BenchmarkListMatcher_BindMatches(b *testing.B) {
	b.ReportAllocs()
	matcher := matching.NewListMatcher(matching.NewConstantMatcher(), matching.NewFieldMatcher(), matching.NewConstantMatcher())
	in := []any{
		&values.ConstantValue{Value: int64(1), Typ: values.NullableLong},
		&values.FieldValue{Field: "x", Typ: values.NullableLong},
		&values.ConstantValue{Value: int64(2), Typ: values.NullableLong},
	}
	outer := matching.NewBindings()
	if got := matcher.BindMatches(outer, in); len(got) == 0 {
		b.Fatal("ListMatcher did not match — the benchmark would time a rejection, not a bind")
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = matcher.BindMatches(outer, in)
	}
}

// BenchmarkAllElementsMatcher_BindMatches measures the per-element
// cost of the all-same-downstream matcher. 5 elements, all match.
func BenchmarkAllElementsMatcher_BindMatches(b *testing.B) {
	b.ReportAllocs()
	matcher := matching.NewAllElementsMatcher(matching.NewConstantMatcher())
	in := []any{
		&values.ConstantValue{Value: int64(1), Typ: values.NullableLong},
		&values.ConstantValue{Value: int64(2), Typ: values.NullableLong},
		&values.ConstantValue{Value: int64(3), Typ: values.NullableLong},
		&values.ConstantValue{Value: int64(4), Typ: values.NullableLong},
		&values.ConstantValue{Value: int64(5), Typ: values.NullableLong},
	}
	outer := matching.NewBindings()
	if got := matcher.BindMatches(outer, in); len(got) == 0 {
		b.Fatal("AllElementsMatcher did not match — the benchmark would time a rejection, not a bind")
	}
	b.ResetTimer()
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
	a := &values.FieldValue{Field: "a", Typ: values.NullableLong}
	bb := &values.FieldValue{Field: "b", Typ: values.NullableLong}
	build := func() predicates.QueryPredicate {
		return predicates.NewNot(predicates.NewAnd(
			predicates.NewComparisonPredicate(a, predicates.Comparison{Type: predicates.ComparisonEquals, Operand: values.LiteralValue(int64(1))}),
			predicates.NewComparisonPredicate(bb, predicates.Comparison{Type: predicates.ComparisonEquals, Operand: values.LiteralValue(int64(2))}),
		))
	}
	rules := NormalizationRules()
	// De Morgan must turn the NOT(AND(...)) into an OR. Timing an input the
	// rules no longer rewrite would misreport the normalization cost as cheap.
	if got, ok := Simplify(build(), rules).(*predicates.OrPredicate); !ok {
		b.Fatalf("Simplify = %T; want *predicates.OrPredicate (De Morgan)", got)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Fresh tree per iter — Simplify mutates via rebuild.
		_ = Simplify(build(), rules)
	}
}

// Opaque-input baseline: the driver fires through every rule but
// nothing yields. Measures the pure-dispatch overhead the planner
// pays per predicate that can't be folded.
func BenchmarkSimplify_NoOp(b *testing.B) {
	b.ReportAllocs()
	pred := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "age", Typ: values.NullableLong},
		predicates.Comparison{Type: predicates.ComparisonGreaterThanEq, Operand: values.LiteralValue(int64(18))},
	)
	rules := DefaultSimplifyRules()
	// The point of this benchmark is that NOTHING yields: the driver must walk
	// every rule and hand back the identical predicate. If some rule starts
	// rewriting it, this stops being the pure-dispatch baseline it claims to be.
	if got := Simplify(pred, rules); got != predicates.QueryPredicate(pred) {
		b.Fatalf("Simplify rewrote the opaque predicate to %T %s; the no-op baseline is no longer a no-op", got, got.Explain())
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = Simplify(pred, rules)
	}
}

// --- B5 / B1 expression-rule benchmarks --------------------------------

// BenchmarkFireExpressionRule_FilterMerge exercises the per-rule hot
// path: matcher binds, OnMatch yields, Reference dedups.
func BenchmarkFireExpressionRule_FilterMerge(b *testing.B) {
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	innerF := expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{pT}, scanQ)
	innerQ := expressions.ForEachQuantifier(expressions.InitialOf(innerF))
	outerF := expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{pT}, innerQ)
	rule := NewFilterMergeRule()
	// A rule that stops matching yields nothing and returns almost instantly —
	// the fastest possible "hot path" and a pure regression. Each iteration
	// builds a fresh Reference, so validate every one.
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ref := expressions.InitialOf(outerF) // fresh ref each iter
		if yielded := FireExpressionRule(rule, ref); len(yielded) == 0 {
			b.Fatal("FilterMergeRule yielded nothing — the rule no longer fires on nested filters")
		}
	}
}

// BenchmarkExpressionMatcher_BindMatch — the per-call match cost.
func BenchmarkExpressionMatcher_BindMatch(b *testing.B) {
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	f := expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{pT}, scanQ)
	matcher := NewExpressionMatcher[*expressions.LogicalFilterExpression]("logical_filter")
	outer := matching.NewBindings()
	if got := matcher.BindMatches(outer, f); len(got) == 0 {
		b.Fatal("ExpressionMatcher did not match the filter — the benchmark would time the type-assert rejection")
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = matcher.BindMatches(outer, f)
	}
}

// BenchmarkOptimise_StackedSorts exercises SortMergeRule +
// DistinctOverSortElim cooperation on:
//
//	Distinct → Sort(k1) → Sort(k2) → Sort(k3) → Scan(Order)
//
// Optimal output is Distinct(Scan) (DistinctOverSortElim absorbs
// the entire Sort stack iteratively). Pins the cooperation cost
// for that rewrite chain.
func BenchmarkOptimise_StackedSorts(b *testing.B) {
	build := func() *expressions.Reference {
		scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
		scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
		k3 := []expressions.SortKey{{Value: &values.FieldValue{Field: "k3", Typ: values.UnknownType}}}
		s3 := expressions.NewLogicalSortExpression(k3, scanQ)
		s3Q := expressions.ForEachQuantifier(expressions.InitialOf(s3))
		k2 := []expressions.SortKey{{Value: &values.FieldValue{Field: "k2", Typ: values.UnknownType}}}
		s2 := expressions.NewLogicalSortExpression(k2, s3Q)
		s2Q := expressions.ForEachQuantifier(expressions.InitialOf(s2))
		k1 := []expressions.SortKey{{Value: &values.FieldValue{Field: "k1", Typ: values.UnknownType}}}
		s1 := expressions.NewLogicalSortExpression(k1, s2Q)
		s1Q := expressions.ForEachQuantifier(expressions.InitialOf(s1))
		d := expressions.NewLogicalDistinctExpression(s1Q)
		return expressions.InitialOf(d)
	}
	rules := DefaultExpressionRules()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ref := build()
		p := NewPlanner(rules, nil)
		// converged=false means the run hit MaxTasks: the sample would then
		// measure cap-clamped work, not the rewrite chain this claims to pin.
		if tasks, converged := exploreRewriting(p, ref); !converged || tasks == 0 {
			b.Fatalf("exploreRewriting: tasks=%d converged=%v; want tasks>0 and convergence", tasks, converged)
		}
	}
}

// BenchmarkOptimise_GetBest pins the cost-driven extraction step:
// optimise a tree to convergence, then call Reference.GetBest with
// the cost-based comparator to pull out the cheapest member.
//
// The build is the same RealisticTree shape as
// BenchmarkPlanner_RealisticTree — five operators with a Filter +
// Distinct + Sort cascade — so the per-iteration delta vs that
// benchmark is exactly the GetBest call, not the optimiser run.
func BenchmarkOptimise_GetBest(b *testing.B) {
	build := func() *expressions.Reference {
		scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
		scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
		innerD := expressions.NewLogicalDistinctExpression(scanQ)
		innerDQ := expressions.ForEachQuantifier(expressions.InitialOf(innerD))
		outerD := expressions.NewLogicalDistinctExpression(innerDQ)
		outerDQ := expressions.ForEachQuantifier(expressions.InitialOf(outerD))
		pT := predicates.NewConstantPredicate(predicates.TriTrue)
		innerF := expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{pT}, outerDQ)
		innerFQ := expressions.ForEachQuantifier(expressions.InitialOf(innerF))
		outerF := expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{pT}, innerFQ)
		outerFQ := expressions.ForEachQuantifier(expressions.InitialOf(outerF))
		topD := expressions.NewLogicalDistinctExpression(outerFQ)
		return expressions.InitialOf(topD)
	}
	rules := DefaultExpressionRules()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ref := build()
		p := NewPlanner(rules, nil)
		if tasks, converged := exploreRewriting(p, ref); !converged || tasks == 0 {
			b.Fatalf("exploreRewriting: tasks=%d converged=%v; want tasks>0 and convergence", tasks, converged)
		}
		if best := ref.GetBest(properties.CostLess); best == nil {
			b.Fatal("GetBest returned nil — an empty Reference makes the extraction free")
		}
	}
}

// BenchmarkPlanner_RealisticTree exercises the task-stack Planner's
// REWRITING exploration on a ~6-node query tree representative of a
// small SELECT (Distinct/Filter/Distinct/Distinct/Scan cascade).
func BenchmarkPlanner_RealisticTree(b *testing.B) {
	build := func() *expressions.Reference {
		scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
		scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
		innerD := expressions.NewLogicalDistinctExpression(scanQ)
		innerDQ := expressions.ForEachQuantifier(expressions.InitialOf(innerD))
		outerD := expressions.NewLogicalDistinctExpression(innerDQ)
		outerDQ := expressions.ForEachQuantifier(expressions.InitialOf(outerD))
		pT := predicates.NewConstantPredicate(predicates.TriTrue)
		innerF := expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{pT}, outerDQ)
		innerFQ := expressions.ForEachQuantifier(expressions.InitialOf(innerF))
		outerF := expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{pT}, innerFQ)
		outerFQ := expressions.ForEachQuantifier(expressions.InitialOf(outerF))
		topD := expressions.NewLogicalDistinctExpression(outerFQ)
		return expressions.InitialOf(topD)
	}
	rules := DefaultExpressionRules()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ref := build()
		p := NewPlanner(rules, nil)
		if tasks, converged := exploreRewriting(p, ref); !converged || tasks == 0 {
			b.Fatalf("exploreRewriting: tasks=%d converged=%v; want tasks>0 and convergence", tasks, converged)
		}
	}
}

// BenchmarkPlanner_FullPlan exercises Plan() on the same shape —
// EXPLORE + ExtractBestPlan. Captures the OPTIMIZE-phase overhead
// on top of EXPLORE.
func BenchmarkPlanner_FullPlan(b *testing.B) {
	build := func() *expressions.Reference {
		scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
		scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
		innerD := expressions.NewLogicalDistinctExpression(scanQ)
		innerDQ := expressions.ForEachQuantifier(expressions.InitialOf(innerD))
		pT := predicates.NewConstantPredicate(predicates.TriTrue)
		f := expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{pT}, innerDQ)
		return expressions.InitialOf(f)
	}
	rules := DefaultExpressionRules()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ref := build()
		p := NewPlanner(rules, nil)
		// Planning that FAILS is instant, so an unchecked Plan turns a
		// correctness regression into a headline speedup.
		best, tasks, err := p.Plan(ref)
		if err != nil {
			b.Fatalf("Plan: %v", err)
		}
		if best == nil {
			b.Fatal("Plan returned no expression")
		}
		if tasks == 0 {
			b.Fatal("Plan ran zero tasks")
		}
	}
}

// BenchmarkBestRefCost pins the cost-only extraction call in
// isolation (no optimiser). Useful baseline for B6's task-stack
// planner perf budget.
func BenchmarkBestRefCost(b *testing.B) {
	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	f := expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{pT}, scanQ)
	d := expressions.NewLogicalDistinctExpression(expressions.ForEachQuantifier(expressions.InitialOf(f)))
	ref := expressions.InitialOf(d)
	// Insert a few alternatives so GetBest does real work.
	ref.Insert(expressions.NewLogicalSortExpression(nil, expressions.ForEachQuantifier(expressions.InitialOf(f))))
	ref.Insert(expressions.NewLogicalDistinctExpression(scanQ))
	// GetBest over an empty Reference returns nil immediately; the loop below
	// re-reads one fixed Reference, so a single pre-loop check covers it.
	if best := ref.GetBest(properties.CostLess); best == nil {
		b.Fatal("GetBest returned nil — the Reference holds no members to compare")
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = ref.GetBest(properties.CostLess)
	}
}

func BenchmarkMemo_MemoizeExpression_LeafHit(b *testing.B) {
	m := NewMemo(nil)
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	want := m.MemoizeExpression(scan)

	// "Hit" is the whole claim: the equivalent leaf must resolve to the
	// ALREADY-memoized Reference. If dedup regresses this becomes a miss —
	// a different code path with a different cost, under a name that lies.
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
		if r := m.MemoizeExpression(s); r != want {
			b.Fatal("MemoizeExpression missed — the leaf was not deduped onto the existing Reference")
		}
	}
}

func BenchmarkMemo_MemoizeExpression_NonLeafHit(b *testing.B) {
	m := NewMemo(nil)
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := m.MemoizeExpression(scan)
	pred := []predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)}
	filter := expressions.NewLogicalFilterExpression(pred, expressions.ForEachQuantifier(scanRef))
	want := m.MemoizeExpression(filter)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		f := expressions.NewLogicalFilterExpression(pred, expressions.ForEachQuantifier(scanRef))
		if r := m.MemoizeExpression(f); r != want {
			b.Fatal("MemoizeExpression missed — the non-leaf was not deduped onto the existing Reference")
		}
	}
}

func BenchmarkPlanner_ExploreWithMemo(b *testing.B) {
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
		scanRef := expressions.InitialOf(scan)
		sort := expressions.NewLogicalSortExpression(nil, expressions.ForEachQuantifier(scanRef))
		sortRef := expressions.InitialOf(sort)
		pred := predicates.NewConstantPredicate(predicates.TriTrue)
		filter := expressions.NewLogicalFilterExpression(
			[]predicates.QueryPredicate{pred},
			expressions.ForEachQuantifier(sortRef),
		)
		rootRef := expressions.InitialOf(filter)
		p := NewPlanner(DefaultExpressionRules(), nil)
		if tasks, converged := exploreRewriting(p, rootRef); !converged || tasks == 0 {
			b.Fatalf("exploreRewriting: tasks=%d converged=%v; want tasks>0 and convergence", tasks, converged)
		}
	}
}

func BenchmarkPlanner_PlanWithIndexCandidates(b *testing.B) {
	a1 := values.UniqueCorrelationIdentifier()
	a2 := values.UniqueCorrelationIdentifier()
	cand := newKnownDistinctValueIndexCandidate(
		"T$a_b",
		[]string{"T"},
		[]string{"A", "B"},
		[]values.CorrelationIdentifier{a1, a2},
		values.UnknownType,
		false,
		nil,
	)
	ctx := &indexTestPlanContext{candidates: []MatchCandidate{cand}}
	rules := DefaultExpressionRules()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
		scanRef := expressions.InitialOf(scan)
		q := expressions.ForEachQuantifier(scanRef)
		filter := expressions.NewLogicalFilterExpression(
			[]predicates.QueryPredicate{
				predicates.NewComparisonPredicate(
					&values.FieldValue{Field: "A", Typ: values.NullableLong},
					predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(1)),
				),
				predicates.NewComparisonPredicate(
					&values.FieldValue{Field: "B", Typ: values.NullableLong},
					predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, int64(10)),
				),
			},
			q,
		)
		filterRef := expressions.InitialOf(filter)
		filterQ := expressions.ForEachQuantifier(filterRef)
		sort := expressions.NewLogicalSortExpression(
			[]expressions.SortKey{{Value: &values.FieldValue{Field: "B", Typ: values.UnknownType}}},
			filterQ,
		)
		ref := expressions.InitialOf(sort)

		p := NewPlanner(rules, ctx).
			WithPlanningExpressionRules(BatchAExpressionRules()).
			WithImplementationRules(DefaultImplementationRules())
		mustPlanPhysical(b, p, ref)
	}
}

func BenchmarkPlanner_PlanAggregation(b *testing.B) {
	a1 := values.UniqueCorrelationIdentifier()
	cand := newKnownDistinctValueIndexCandidate(
		"T$region",
		[]string{"T"},
		[]string{"region"},
		[]values.CorrelationIdentifier{a1},
		values.UnknownType,
		false,
		nil,
	)
	ctx := &indexTestPlanContext{candidates: []MatchCandidate{cand}}
	rules := DefaultExpressionRules()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
		scanRef := expressions.InitialOf(scan)
		scanQ := expressions.ForEachQuantifier(scanRef)
		sort := expressions.NewLogicalSortExpression(
			[]expressions.SortKey{{Value: &values.FieldValue{Field: "region", Typ: values.UnknownType}}},
			scanQ,
		)
		sortRef := expressions.InitialOf(sort)
		sortQ := expressions.ForEachQuantifier(sortRef)
		gb := expressions.NewGroupByExpression(
			[]values.Value{&values.FieldValue{Field: "region", Typ: values.UnknownType}},
			[]expressions.AggregateSpec{
				{Function: expressions.AggCount, Operand: &values.FieldValue{Field: "id", Typ: values.UnknownType}},
			},
			sortQ,
		)
		ref := expressions.InitialOf(gb)

		p := NewPlanner(rules, ctx).
			WithPlanningExpressionRules(BatchAExpressionRules()).
			WithImplementationRules(DefaultImplementationRules())
		mustPlanPhysical(b, p, ref)
	}
}

func BenchmarkPlanner_PlanAggregationFromIndex(b *testing.B) {
	a1 := values.UniqueCorrelationIdentifier()
	cand := newKnownDistinctValueIndexCandidate(
		"T$region",
		[]string{"T"},
		[]string{"region"},
		[]values.CorrelationIdentifier{a1},
		values.UnknownType,
		false,
		nil,
	)
	ctx := &indexTestPlanContext{candidates: []MatchCandidate{cand}}
	rules := DefaultExpressionRules()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
		scanRef := expressions.InitialOf(scan)
		scanQ := expressions.ForEachQuantifier(scanRef)
		gb := expressions.NewGroupByExpression(
			[]values.Value{&values.FieldValue{Field: "region", Typ: values.UnknownType}},
			[]expressions.AggregateSpec{
				{Function: expressions.AggCount, Operand: &values.FieldValue{Field: "id", Typ: values.UnknownType}},
			},
			scanQ,
		)
		ref := expressions.InitialOf(gb)

		p := NewPlanner(rules, ctx).
			WithPlanningExpressionRules(BatchAExpressionRules()).
			WithImplementationRules(DefaultImplementationRules())
		mustPlanPhysical(b, p, ref)
	}
}

// mustPlanPhysical runs one full planning pass and fails the benchmark unless
// it produced a PHYSICAL plan. Plan can also return a logical expression with a
// nil error — no implementation rule realized the tree — which costs strictly
// less than real planning, so checking only the error would still let an
// implementation-rule regression register as a speedup.
//
// Called inside the timed loop on purpose: each iteration builds a fresh
// Reference and a fresh single-use Planner, so nothing outside the loop can
// vouch for them. It costs one type check and two comparisons against a
// microsecond-scale planning run, and every revision under comparison pays it
// identically, so it cannot fabricate or hide a delta.
func mustPlanPhysical(b *testing.B, p *Planner, ref *expressions.Reference) {
	b.Helper()
	best, tasks, err := p.Plan(ref)
	if err != nil {
		b.Fatalf("Plan: %v", err)
	}
	if best == nil {
		b.Fatal("Plan returned no expression")
	}
	if tasks == 0 {
		b.Fatal("Plan ran zero tasks")
	}
	if !isPhysical(best) {
		b.Fatalf("Plan returned a non-physical best expression %T — no implementation rule realized the tree", best)
	}
}
