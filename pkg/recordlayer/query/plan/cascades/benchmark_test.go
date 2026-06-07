package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/properties"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// Micro-benchmarks for the Phase 4.0 cascades seed. These aren't
// a performance gate today — they're here so subsequent shifts can
// detect regressions as the real Value + predicate hierarchies
// land.

func BenchmarkConstantValue_Evaluate(b *testing.B) {
	v := &values.ConstantValue{Value: int64(42), Typ: values.TypeInt}
	for i := 0; i < b.N; i++ {
		_, _ = v.Evaluate(nil)
	}
}

func BenchmarkFieldValue_Evaluate(b *testing.B) {
	v := &values.FieldValue{Field: "age", Typ: values.TypeInt}
	row := map[string]any{"age": int64(30)}
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
			Left:  &values.FieldValue{Field: "a", Typ: values.TypeInt},
			Right: &values.FieldValue{Field: "b", Typ: values.TypeInt},
		},
		Right: &values.ArithmeticValue{
			Op:    values.OpSub,
			Left:  &values.FieldValue{Field: "c", Typ: values.TypeInt},
			Right: &values.FieldValue{Field: "d", Typ: values.TypeInt},
		},
	}
	row := map[string]any{"a": int64(3), "b": int64(4), "c": int64(10), "d": int64(5)}
	for i := 0; i < b.N; i++ {
		_, _ = v.Evaluate(row)
	}
}

func BenchmarkComparisonPredicate_Eval(b *testing.B) {
	pred := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "age", Typ: values.TypeInt},
		predicates.Comparison{Type: predicates.ComparisonGreaterThanEq, Operand: values.LiteralValue(int64(18))},
	)
	row := map[string]any{"age": int64(30)}
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
		&values.FieldValue{Field: "age", Typ: values.TypeInt},
		predicates.Comparison{Type: predicates.ComparisonEquals, Operand: &values.FieldValue{Field: "cutoff", Typ: values.TypeInt}},
	)
	row := map[string]any{"age": int64(18), "cutoff": int64(18)}
	for i := 0; i < b.N; i++ {
		_, _ = pred.Eval(row)
	}
}

func BenchmarkKleeneAnd_Eval(b *testing.B) {
	// (age >= 18) AND (rank < 5) AND (score > 50)
	tree := predicates.NewAnd(
		predicates.NewComparisonPredicate(&values.FieldValue{Field: "age", Typ: values.TypeInt},
			predicates.Comparison{Type: predicates.ComparisonGreaterThanEq, Operand: values.LiteralValue(int64(18))}),
		predicates.NewComparisonPredicate(&values.FieldValue{Field: "rank", Typ: values.TypeInt},
			predicates.Comparison{Type: predicates.ComparisonLessThan, Operand: values.LiteralValue(int64(5))}),
		predicates.NewComparisonPredicate(&values.FieldValue{Field: "score", Typ: values.TypeInt},
			predicates.Comparison{Type: predicates.ComparisonGreaterThan, Operand: values.LiteralValue(int64(50))}),
	)
	row := map[string]any{"age": int64(30), "rank": int64(3), "score": int64(80)}
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
		Left:  &values.ConstantValue{Value: int64(5), Typ: values.TypeInt},
		Right: &values.FieldValue{Field: "x", Typ: values.TypeInt},
	}
	outer := matching.NewBindings()
	for i := 0; i < b.N; i++ {
		_ = matcher.BindMatches(outer, expr)
	}
}

func BenchmarkAllOf_BindMatches(b *testing.B) {
	b.ReportAllocs()
	// AllOf(ConstantMatcher, AnyValue) against a ConstantValue.
	pattern := matching.NewAllOf("ConstantValue", matching.NewConstantMatcher(), matching.NewAnyValue())
	cv := &values.ConstantValue{Value: int64(7), Typ: values.TypeInt}
	outer := matching.NewBindings()
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
		&values.FieldValue{Field: "age", Typ: values.TypeInt},
		predicates.Comparison{Type: predicates.ComparisonGreaterThanEq, Operand: values.LiteralValue(int64(18))},
	)
	// Build fresh each iter — Simplify sees a pristine tree, not a
	// memoised folded one.
	rules := DefaultSimplifyRules()
	for i := 0; i < b.N; i++ {
		pred := predicates.NewAnd(
			predicates.NewAnd(
				predicates.NewComparisonPredicate(
					&values.ConstantValue{Value: int64(5), Typ: values.TypeInt},
					predicates.Comparison{Type: predicates.ComparisonEquals, Operand: values.LiteralValue(int64(5))},
				),
				predicates.NewNot(predicates.NewNot(predicates.NewConstantPredicate(predicates.TriTrue))),
			),
			agePred,
			agePred,
			predicates.NewConstantPredicate(predicates.TriTrue),
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
	p := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "a", Typ: values.TypeInt},
		predicates.Comparison{Type: predicates.ComparisonEquals, Operand: values.LiteralValue(int64(1))},
	)
	q := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "b", Typ: values.TypeInt},
		predicates.Comparison{Type: predicates.ComparisonEquals, Operand: values.LiteralValue(int64(2))},
	)
	rules := DefaultSimplifyRules()
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
		&values.ConstantValue{Value: int64(1), Typ: values.TypeInt},
		&values.FieldValue{Field: "x", Typ: values.TypeInt},
		&values.ConstantValue{Value: int64(2), Typ: values.TypeInt},
	}
	outer := matching.NewBindings()
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
		&values.ConstantValue{Value: int64(1), Typ: values.TypeInt},
		&values.ConstantValue{Value: int64(2), Typ: values.TypeInt},
		&values.ConstantValue{Value: int64(3), Typ: values.TypeInt},
		&values.ConstantValue{Value: int64(4), Typ: values.TypeInt},
		&values.ConstantValue{Value: int64(5), Typ: values.TypeInt},
	}
	outer := matching.NewBindings()
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
	a := &values.FieldValue{Field: "a", Typ: values.TypeInt}
	bb := &values.FieldValue{Field: "b", Typ: values.TypeInt}
	rules := NormalizationRules()
	for i := 0; i < b.N; i++ {
		// Fresh tree per iter — Simplify mutates via rebuild.
		pred := predicates.NewNot(predicates.NewAnd(
			predicates.NewComparisonPredicate(a, predicates.Comparison{Type: predicates.ComparisonEquals, Operand: values.LiteralValue(int64(1))}),
			predicates.NewComparisonPredicate(bb, predicates.Comparison{Type: predicates.ComparisonEquals, Operand: values.LiteralValue(int64(2))}),
		))
		_ = Simplify(pred, rules)
	}
}

// Opaque-input baseline: the driver fires through every rule but
// nothing yields. Measures the pure-dispatch overhead the planner
// pays per predicate that can't be folded.
func BenchmarkSimplify_NoOp(b *testing.B) {
	b.ReportAllocs()
	pred := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "age", Typ: values.TypeInt},
		predicates.Comparison{Type: predicates.ComparisonGreaterThanEq, Operand: values.LiteralValue(int64(18))},
	)
	rules := DefaultSimplifyRules()
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
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ref := expressions.InitialOf(outerF) // fresh ref each iter
		_ = FireExpressionRule(rule, ref)
	}
}

// BenchmarkFixpointApply_DefaultRules drives the full default rule
// set via FixpointApply on a small test tree.
func BenchmarkFixpointApply_DefaultRules(b *testing.B) {
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	innerD := expressions.NewLogicalDistinctExpression(scanQ)
	innerDQ := expressions.ForEachQuantifier(expressions.InitialOf(innerD))
	outerD := expressions.NewLogicalDistinctExpression(innerDQ)
	rules := DefaultExpressionRules()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ref := expressions.InitialOf(outerD)
		_, _ = FixpointApply(rules, ref, 50)
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
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = matcher.BindMatches(outer, f)
	}
}

// BenchmarkOptimise_RealisticTree drives the full default rule set
// (FixpointApply with all 31 logical-rewrite rules + sub-Reference
// descent) on a ~6-node query tree representative of a small SELECT:
//
//	Distinct
//	  → Filter([T])
//	    → Filter([T])
//	      → Distinct
//	        → Distinct
//	          → Scan(Order)
//
// Pins the end-to-end cost of one optimisation pass — useful as a
// macro-benchmark when future tuning changes rule order or adds
// per-rule short-circuits.
func BenchmarkOptimise_RealisticTree(b *testing.B) {
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
		_, _ = FixpointApply(rules, ref, 50)
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
		_, _ = FixpointApply(rules, ref, 50)
	}
}

// BenchmarkOptimise_GetBest pins the Track B4 cost-driven extraction
// step: optimise a tree to convergence, then call Reference.GetBest
// with the cost-based comparator to pull out the cheapest member.
//
// The build is the same RealisticTree shape as
// BenchmarkOptimise_RealisticTree — five operators with a Filter +
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
		_, _ = FixpointApply(rules, ref, 50)
		_ = ref.GetBest(properties.CostLess)
	}
}

// BenchmarkPlanner_RealisticTree exercises the new B6 task-stack
// Planner on the same RealisticTree shape as
// BenchmarkOptimise_RealisticTree (FixpointApply baseline). Direct
// comparison reveals the saturation-tracking perf savings.
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
		_, _ = p.Explore(ref)
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
		_, _, _ = p.Plan(ref)
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
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = ref.GetBest(properties.CostLess)
	}
}

func BenchmarkMemo_MemoizeExpression_LeafHit(b *testing.B) {
	m := NewMemo(nil)
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	m.MemoizeExpression(scan)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
		m.MemoizeExpression(s)
	}
}

func BenchmarkMemo_MemoizeExpression_NonLeafHit(b *testing.B) {
	m := NewMemo(nil)
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := m.MemoizeExpression(scan)
	pred := []predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)}
	filter := expressions.NewLogicalFilterExpression(pred, expressions.ForEachQuantifier(scanRef))
	m.MemoizeExpression(filter)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		f := expressions.NewLogicalFilterExpression(pred, expressions.ForEachQuantifier(scanRef))
		m.MemoizeExpression(f)
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
		p.Explore(rootRef)
	}
}

func BenchmarkPlanner_PlanWithIndexCandidates(b *testing.B) {
	a1 := values.UniqueCorrelationIdentifier()
	a2 := values.UniqueCorrelationIdentifier()
	cand := NewValueIndexScanMatchCandidate(
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
					&values.FieldValue{Field: "A", Typ: values.TypeInt},
					predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(1)),
				),
				predicates.NewComparisonPredicate(
					&values.FieldValue{Field: "B", Typ: values.TypeInt},
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
		p.Plan(ref)
	}
}

func BenchmarkPlanner_PlanAggregation(b *testing.B) {
	a1 := values.UniqueCorrelationIdentifier()
	cand := NewValueIndexScanMatchCandidate(
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
		p.Plan(ref)
	}
}

func BenchmarkPlanner_PlanAggregationFromIndex(b *testing.B) {
	a1 := values.UniqueCorrelationIdentifier()
	cand := NewValueIndexScanMatchCandidate(
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
		p.Plan(ref)
	}
}
