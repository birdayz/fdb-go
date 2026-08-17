package cascades

import (
	"context"
	"errors"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

var errRuleProbe = errors.New("rule probe failed")

func mustFireRule(t testing.TB, rule CascadesRule, in any) []any {
	t.Helper()
	got, err := FireRule(rule, in)
	if err != nil {
		t.Fatalf("FireRule() unexpected error: %v", err)
	}
	return got
}

func mustSimplify(
	t testing.TB,
	pred predicates.QueryPredicate,
	rules []CascadesRule,
) predicates.QueryPredicate {
	t.Helper()
	got, err := Simplify(pred, rules)
	if err != nil {
		t.Fatalf("Simplify() unexpected error: %v", err)
	}
	return got
}

func mustFireExpressionRule(
	t testing.TB,
	rule ExpressionRule,
	ref *expressions.Reference,
) []expressions.RelationalExpression {
	t.Helper()
	got, err := FireExpressionRule(rule, ref)
	if err != nil {
		t.Fatalf("FireExpressionRule() unexpected error: %v", err)
	}
	return got
}

func mustFireExpressionRuleWithMemo(
	t testing.TB,
	rule ExpressionRule,
	ref *expressions.Reference,
	ctx PlanContext,
	memo *Memo,
) []expressions.RelationalExpression {
	t.Helper()
	got, err := FireExpressionRuleWithMemo(rule, ref, ctx, memo)
	if err != nil {
		t.Fatalf("FireExpressionRuleWithMemo() unexpected error: %v", err)
	}
	return got
}

func mustFireImplementationRule(
	t testing.TB,
	rule ImplementationRule,
	ref *expressions.Reference,
	constraints ...*ConstraintMap,
) []expressions.RelationalExpression {
	t.Helper()
	got, err := FireImplementationRule(rule, ref, constraints...)
	if err != nil {
		t.Fatalf("FireImplementationRule() unexpected error: %v", err)
	}
	return got
}

func mustFireImplementationRuleWithContext(
	t testing.TB,
	rule ImplementationRule,
	ref *expressions.Reference,
	ctx PlanContext,
	memo *Memo,
	constraints ...*ConstraintMap,
) []expressions.RelationalExpression {
	t.Helper()
	got, err := FireImplementationRuleWithContext(rule, ref, ctx, memo, constraints...)
	if err != nil {
		t.Fatalf("FireImplementationRuleWithContext() unexpected error: %v", err)
	}
	return got
}

// mustRunImplementationRuleCall drives the same stage/check/commit protocol
// as the production implementation-rule drivers for tests that intentionally
// exercise one pre-built binding directly.
func mustRunImplementationRuleCall(
	t testing.TB,
	rule ImplementationRule,
	call *ImplementationRuleCall,
) {
	t.Helper()
	rule.OnMatch(call)
	if err := call.Err(); err != nil {
		t.Fatalf("%T.OnMatch() unexpected error: %v", rule, err)
	}
	call.applyPendingConstraints()
}

type failAfterYieldRule struct {
	matcher matching.BindingMatcher
}

type failAfterYieldExpressionRule struct {
	matcher matching.BindingMatcher
	yield   expressions.RelationalExpression
}

func (r *failAfterYieldExpressionRule) Matcher() matching.BindingMatcher {
	return r.matcher
}

func (r *failAfterYieldExpressionRule) OnMatch(call *ExpressionRuleCall) {
	yield := r.yield
	if yield == nil {
		yield = fixtureScan("merge-target")
	}
	call.Yield(yield)
	call.Fail(errRuleProbe)
}

type failAfterYieldImplementationRule struct {
	matcher matching.BindingMatcher
	child   *expressions.Reference
	yield   expressions.RelationalExpression
}

func (r *failAfterYieldImplementationRule) Matcher() matching.BindingMatcher {
	return r.matcher
}

func (r *failAfterYieldImplementationRule) OnMatch(call *ImplementationRuleCall) {
	yield := r.yield
	if yield == nil {
		var err error
		yield, err = plans.NewRecordQueryScanPlan([]string{"P"}, values.NotNullLong, false)
		if err != nil {
			call.Fail(err)
			return
		}
	}
	call.Yield(yield)
	call.PushConstraint(r.child, []*properties.RequestedOrdering{
		properties.NewRequestedOrdering(nil, properties.DistinctnessNotDistinct, false),
	})
	call.Fail(errRuleProbe)
}

type successfulBatchExpressionRule struct {
	matcher matching.BindingMatcher
	yields  []expressions.RelationalExpression
}

func (r *successfulBatchExpressionRule) Matcher() matching.BindingMatcher {
	return r.matcher
}

func (r *successfulBatchExpressionRule) OnMatch(call *ExpressionRuleCall) {
	for _, yield := range r.yields {
		call.Yield(yield)
	}
}

type successfulBatchImplementationRule struct {
	matcher matching.BindingMatcher
	child   *expressions.Reference
	yields  []expressions.RelationalExpression
}

func (r *successfulBatchImplementationRule) Matcher() matching.BindingMatcher {
	return r.matcher
}

func (r *successfulBatchImplementationRule) OnMatch(call *ImplementationRuleCall) {
	for _, yield := range r.yields {
		call.Yield(yield)
	}
	call.PushConstraint(r.child, []*properties.RequestedOrdering{
		properties.NewRequestedOrdering(nil, properties.DistinctnessNotDistinct, false),
	})
}

type failOnSwappedImplementationRule struct {
	matcher    *ExpressionMatcher[*expressions.SelectExpression]
	child      *expressions.Reference
	yield      expressions.RelationalExpression
	sawPrimary bool
	sawSwapped bool
}

func (r *failOnSwappedImplementationRule) Matcher() matching.BindingMatcher {
	return r.matcher
}

func (r *failOnSwappedImplementationRule) OnMatch(call *ImplementationRuleCall) {
	selectExpr := matching.Get[*expressions.SelectExpression](call.Bindings, r.matcher)
	if !selectExpr.IsQuantifiersSwapped() {
		r.sawPrimary = true
		return
	}
	r.sawSwapped = true
	call.Yield(r.yield)
	call.PushConstraint(r.child, []*properties.RequestedOrdering{
		properties.NewRequestedOrdering(nil, properties.DistinctnessNotDistinct, false),
	})
	call.Fail(errRuleProbe)
}

func (r *failAfterYieldRule) Matcher() matching.BindingMatcher {
	return r.matcher
}

func (r *failAfterYieldRule) OnMatch(call *RuleCall) {
	call.Yield(predicates.NewConstantPredicate(predicates.TriTrue))
	call.Fail(errRuleProbe)
}

func TestFireRule_LateFailureDiscardsEveryYield(t *testing.T) {
	t.Parallel()

	in := predicates.NewConstantPredicate(predicates.TriFalse)
	rule := &failAfterYieldRule{
		matcher: &predicateMatcher[predicates.QueryPredicate]{rootType: "predicate"},
	}

	got, err := FireRule(rule, in)
	if !errors.Is(err, errRuleProbe) {
		t.Fatalf("FireRule error = %v, want %v", err, errRuleProbe)
	}
	if got != nil {
		t.Fatalf("FireRule yielded %v after failure, want nil", got)
	}
}

func TestSimplify_LateRuleFailureReturnsNoPredicate(t *testing.T) {
	t.Parallel()

	in := predicates.NewConstantPredicate(predicates.TriFalse)
	rule := &failAfterYieldRule{
		matcher: &predicateMatcher[predicates.QueryPredicate]{rootType: "predicate"},
	}

	got, err := Simplify(in, []CascadesRule{rule})
	if !errors.Is(err, errRuleProbe) {
		t.Fatalf("Simplify error = %v, want %v", err, errRuleProbe)
	}
	if got != nil {
		t.Fatalf("Simplify returned %v after failure, want nil", got)
	}
}

func TestRuleCall_FirstFailureWinsAndSuppressesLaterYields(t *testing.T) {
	t.Parallel()

	call := &RuleCall{Bindings: matching.NewBindings()}
	call.Fail(errRuleProbe)
	call.Fail(errors.New("later error"))
	call.Yield(predicates.NewConstantPredicate(predicates.TriTrue))

	if !errors.Is(call.Err(), errRuleProbe) {
		t.Fatalf("RuleCall.Err() = %v, want first error %v", call.Err(), errRuleProbe)
	}
	if got := call.Yielded(); got != nil {
		t.Fatalf("RuleCall.Yielded() = %v after failure, want nil", got)
	}
}

func TestFireExpressionRuleWithMemo_LateFailureIsAtomic(t *testing.T) {
	t.Parallel()

	root := expressions.InitialOf(fixtureScan("root"))
	other := expressions.InitialOf(fixtureScan("merge-target"))
	memo := NewMemo(root)
	memo.RegisterReference(other)
	beforeRoot := append([]expressions.RelationalExpression(nil), root.Members()...)
	beforeOther := append([]expressions.RelationalExpression(nil), other.Members()...)
	beforeMembers := memo.TotalMembers()
	beforeMerges := memo.MergeCount()
	beforeRefs := len(memo.References())
	beforeRootCanonical := root.Canonical()
	beforeOtherCanonical := other.Canonical()
	rule := &failAfterYieldExpressionRule{
		matcher: NewExpressionMatcher[*expressions.FullUnorderedScanExpression]("fail-expression"),
	}

	got, err := FireExpressionRuleWithMemo(rule, root, EmptyPlanContext(), memo)
	if !errors.Is(err, errRuleProbe) {
		t.Fatalf("FireExpressionRuleWithMemo error = %v, want %v", err, errRuleProbe)
	}
	if got != nil {
		t.Fatalf("yielded %v after failure, want nil", got)
	}
	assertSameExpressionPointers(t, "root members", beforeRoot, root.Members())
	assertSameExpressionPointers(t, "other members", beforeOther, other.Members())
	if memo.TotalMembers() != beforeMembers || memo.MergeCount() != beforeMerges || len(memo.References()) != beforeRefs {
		t.Fatalf("memo changed after failure: members %d→%d merges %d→%d refs %d→%d",
			beforeMembers, memo.TotalMembers(), beforeMerges, memo.MergeCount(), beforeRefs, len(memo.References()))
	}
	if root.Canonical() != beforeRootCanonical || other.Canonical() != beforeOtherCanonical {
		t.Fatal("memo canonical topology changed after failed rule")
	}
}

func TestFireImplementationRule_LateFailureIsAtomic(t *testing.T) {
	t.Parallel()

	root := expressions.InitialOf(fixtureScan("root"))
	child := expressions.InitialOf(fixtureScan("child"))
	cm := NewConstraintMap()
	beforeExploratory := append([]expressions.RelationalExpression(nil), root.Members()...)
	beforeFinal := append([]expressions.RelationalExpression(nil), root.FinalMembers()...)
	beforeTick := child.ConstraintsMap().CurrentTick()
	rule := &failAfterYieldImplementationRule{
		matcher: NewExpressionMatcher[*expressions.FullUnorderedScanExpression]("fail-implementation"),
		child:   child,
	}

	got, err := FireImplementationRule(rule, root, cm)
	if !errors.Is(err, errRuleProbe) {
		t.Fatalf("FireImplementationRule error = %v, want %v", err, errRuleProbe)
	}
	if got != nil {
		t.Fatalf("yielded %v after failure, want nil", got)
	}
	assertSameExpressionPointers(t, "exploratory members", beforeExploratory, root.Members())
	assertSameExpressionPointers(t, "final members", beforeFinal, root.FinalMembers())
	if tick := child.ConstraintsMap().CurrentTick(); tick != beforeTick {
		t.Fatalf("constraint tick changed after failure: %d → %d", beforeTick, tick)
	}
	if _, ok := Get(cm, child, RequestedOrderingConstraintKey); ok {
		t.Fatal("failed rule published a child constraint")
	}
}

func TestExpressionRuleAdapter_LateFailureIsAtomic(t *testing.T) {
	t.Parallel()

	root := expressions.InitialOf(fixtureScan("adapter-root"))
	yield := fixtureScan("adapter-leaked-yield")
	memo := NewMemo(root)
	beforeMembers := append([]expressions.RelationalExpression(nil), root.Members()...)
	beforeFinals := append([]expressions.RelationalExpression(nil), root.FinalMembers()...)
	beforeMemoMembers := memo.TotalMembers()
	beforeMemoRefs := len(memo.References())
	inner := &failAfterYieldExpressionRule{
		matcher: NewExpressionMatcher[*expressions.FullUnorderedScanExpression]("adapter-failure"),
		yield:   yield,
	}

	got, err := FireImplementationRuleWithContext(
		AsImplementationRule(inner),
		root,
		EmptyPlanContext(),
		memo,
	)
	if !errors.Is(err, errRuleProbe) {
		t.Fatalf("adapted rule error = %v, want %v", err, errRuleProbe)
	}
	if got != nil {
		t.Fatalf("adapted rule yielded %v after failure, want nil", got)
	}
	assertSameExpressionPointers(t, "adapter exploratory members", beforeMembers, root.Members())
	assertSameExpressionPointers(t, "adapter final members", beforeFinals, root.FinalMembers())
	assertExpressionPointerCount(t, "adapter exploratory members", root.Members(), yield, 0)
	assertExpressionPointerCount(t, "adapter final members", root.FinalMembers(), yield, 0)
	if memo.TotalMembers() != beforeMemoMembers || len(memo.References()) != beforeMemoRefs {
		t.Fatalf("adapter failure changed memo: members %d→%d refs %d→%d",
			beforeMemoMembers, memo.TotalMembers(), beforeMemoRefs, len(memo.References()))
	}
}

func TestFireExpressionRuleWithMemo_SuccessfulBatchCommitsEachYieldOnce(t *testing.T) {
	t.Parallel()

	root := expressions.InitialOf(fixtureScan("expression-success-root"))
	first := fixtureScan("expression-success-first")
	second := fixtureScan("expression-success-second")
	memo := NewMemo(root)
	rule := &successfulBatchExpressionRule{
		matcher: NewExpressionMatcher[*expressions.FullUnorderedScanExpression]("expression-success"),
		yields:  []expressions.RelationalExpression{first, second},
	}

	got, err := FireExpressionRuleWithMemo(rule, root, EmptyPlanContext(), memo)
	if err != nil {
		t.Fatalf("FireExpressionRuleWithMemo() error = %v", err)
	}
	assertSameExpressionPointers(t, "successful expression batch", []expressions.RelationalExpression{first, second}, got)
	assertExpressionPointerCount(t, "exploratory members", root.Members(), first, 1)
	assertExpressionPointerCount(t, "exploratory members", root.Members(), second, 1)
	if gotMembers := memo.TotalMembers(); gotMembers != 3 {
		t.Fatalf("memo member count after successful two-yield batch = %d, want 3", gotMembers)
	}
}

func TestFireImplementationRule_SuccessfulBatchCommitsYieldsAndConstraintOnce(t *testing.T) {
	t.Parallel()

	root := expressions.InitialOf(fixtureScan("implementation-success-root"))
	child := expressions.InitialOf(fixtureScan("implementation-success-child"))
	first, err := plans.NewRecordQueryScanPlan([]string{"implementation-success-first"}, values.NotNullLong, false)
	first = mustConstruct(t, first, err)
	second, err := plans.NewRecordQueryScanPlan([]string{"implementation-success-second"}, values.NotNullLong, false)
	second = mustConstruct(t, second, err)
	cm := NewConstraintMap()
	beforeTick := child.ConstraintsMap().CurrentTick()
	rule := &successfulBatchImplementationRule{
		matcher: NewExpressionMatcher[*expressions.FullUnorderedScanExpression]("implementation-success"),
		child:   child,
		yields:  []expressions.RelationalExpression{first, second},
	}

	got, err := FireImplementationRule(rule, root, cm)
	if err != nil {
		t.Fatalf("FireImplementationRule() error = %v", err)
	}
	assertSameExpressionPointers(t, "successful implementation batch", []expressions.RelationalExpression{first, second}, got)
	assertExpressionPointerCount(t, "final members", root.FinalMembers(), first, 1)
	assertExpressionPointerCount(t, "final members", root.FinalMembers(), second, 1)
	if gotMembers := len(root.Members()); gotMembers != 1 {
		t.Fatalf("successful implementation batch changed exploratory member count to %d, want 1", gotMembers)
	}
	orderings, ok := Get(cm, child, RequestedOrderingConstraintKey)
	if !ok || len(orderings) != 1 {
		t.Fatalf("successful implementation batch constraint = (%v, %v), want one ordering", orderings, ok)
	}
	if tick := child.ConstraintsMap().CurrentTick(); tick != beforeTick+1 {
		t.Fatalf("successful implementation batch constraint tick = %d, want %d", tick, beforeTick+1)
	}
}

func TestPlanner_TransformExprTaskFailureReachesPlanWithoutLeakingYield(t *testing.T) {
	t.Parallel()

	root := expressions.InitialOf(fixtureScan("planner-expression-root"))
	yield := fixtureScan("planner-expression-leaked-yield")
	rule := &failAfterYieldExpressionRule{
		matcher: NewExpressionMatcher[*expressions.FullUnorderedScanExpression]("planner-expression-failure"),
		yield:   yield,
	}
	planner := NewPlanner([]ExpressionRule{rule}, nil)

	plan, tasks, err := planner.PlanWithContext(context.Background(), root)
	if !errors.Is(err, errRuleProbe) {
		t.Fatalf("PlanWithContext() error = %v, want %v", err, errRuleProbe)
	}
	if plan != nil {
		t.Fatalf("PlanWithContext() plan = %T after rule failure, want nil", plan)
	}
	if tasks == 0 {
		t.Fatal("PlanWithContext() ran no tasks; TransformExprTask fixture did not execute")
	}
	assertExpressionPointerCount(t, "planner expression exploratory members", root.Members(), yield, 0)
	assertExpressionPointerCount(t, "planner expression final members", root.FinalMembers(), yield, 0)
	if gotMembers := planner.Memo().TotalMembers(); gotMembers != 1 {
		t.Fatalf("planner memo has %d members after failed expression task, want original member only", gotMembers)
	}
}

func TestPlanner_TransformImplTaskFailureReachesPlanWithoutLeakingEffects(t *testing.T) {
	t.Parallel()

	root := expressions.InitialOf(fixtureScan("planner-implementation-root"))
	child := expressions.InitialOf(fixtureScan("planner-implementation-constraint-child"))
	yield, err := plans.NewRecordQueryScanPlan([]string{"planner-implementation-leaked-yield"}, values.NotNullLong, false)
	yield = mustConstruct(t, yield, err)
	beforeTick := child.ConstraintsMap().CurrentTick()
	rule := &failAfterYieldImplementationRule{
		matcher: NewExpressionMatcher[*expressions.FullUnorderedScanExpression]("planner-implementation-failure"),
		child:   child,
		yield:   yield,
	}
	planner := NewPlanner(nil, nil).WithImplementationRules([]ImplementationRule{rule})

	plan, tasks, err := planner.PlanWithContext(context.Background(), root)
	if !errors.Is(err, errRuleProbe) {
		t.Fatalf("PlanWithContext() error = %v, want %v", err, errRuleProbe)
	}
	if plan != nil {
		t.Fatalf("PlanWithContext() plan = %T after rule failure, want nil", plan)
	}
	if tasks == 0 {
		t.Fatal("PlanWithContext() ran no tasks; TransformImplTask fixture did not execute")
	}
	assertExpressionPointerCount(t, "planner implementation exploratory members", root.Members(), yield, 0)
	assertExpressionPointerCount(t, "planner implementation final members", root.FinalMembers(), yield, 0)
	if _, ok := Get(planner.constraintMap, child, RequestedOrderingConstraintKey); ok {
		t.Fatal("failed TransformImplTask published its staged child constraint")
	}
	if tick := child.ConstraintsMap().CurrentTick(); tick != beforeTick {
		t.Fatalf("failed TransformImplTask changed child constraint tick: %d → %d", beforeTick, tick)
	}
}

func TestPlanner_SwappedTransformImplTaskFailureReachesPlanWithoutLeakingEffects(t *testing.T) {
	t.Parallel()

	left := expressions.ForEachQuantifier(expressions.InitialOf(fixtureScan("planner-swapped-left")))
	right := expressions.ForEachQuantifier(expressions.InitialOf(fixtureScan("planner-swapped-right")))
	leftResult, err := left.RequireFlowedObjectValue()
	leftResult = mustConstruct(t, leftResult, err)
	selectExpr, err := expressions.NewSelectExpression(
		leftResult,
		[]expressions.Quantifier{left, right},
		nil,
	)
	selectExpr = mustConstruct(t, selectExpr, err)
	root := expressions.InitialOf(selectExpr)
	child := expressions.InitialOf(fixtureScan("planner-swapped-constraint-child"))
	yield, err := plans.NewRecordQueryScanPlan([]string{"planner-swapped-leaked-yield"}, values.NotNullLong, false)
	yield = mustConstruct(t, yield, err)
	beforeTick := child.ConstraintsMap().CurrentTick()
	rule := &failOnSwappedImplementationRule{
		matcher: NewExpressionMatcher[*expressions.SelectExpression]("planner-swapped-implementation-failure"),
		child:   child,
		yield:   yield,
	}
	planner := NewPlanner(nil, nil).WithImplementationRules([]ImplementationRule{rule})

	plan, tasks, err := planner.PlanWithContext(context.Background(), root)
	if !errors.Is(err, errRuleProbe) {
		t.Fatalf("PlanWithContext() error = %v, want swapped-rule failure %v", err, errRuleProbe)
	}
	if plan != nil {
		t.Fatalf("PlanWithContext() plan = %T after swapped rule failure, want nil", plan)
	}
	if tasks == 0 {
		t.Fatal("PlanWithContext() ran no tasks; swapped TransformImplTask fixture did not execute")
	}
	if !rule.sawPrimary || !rule.sawSwapped {
		t.Fatalf("rule invocation coverage = primary:%v swapped:%v, want both", rule.sawPrimary, rule.sawSwapped)
	}
	assertExpressionPointerCount(t, "swapped planner exploratory members", root.Members(), yield, 0)
	assertExpressionPointerCount(t, "swapped planner final members", root.FinalMembers(), yield, 0)
	if _, ok := Get(planner.constraintMap, child, RequestedOrderingConstraintKey); ok {
		t.Fatal("failed swapped TransformImplTask published its staged child constraint")
	}
	if tick := child.ConstraintsMap().CurrentTick(); tick != beforeTick {
		t.Fatalf("failed swapped TransformImplTask changed child constraint tick: %d → %d", beforeTick, tick)
	}
}

func assertExpressionPointerCount(
	t testing.TB,
	label string,
	expressionsToSearch []expressions.RelationalExpression,
	wantExpression expressions.RelationalExpression,
	wantCount int,
) {
	t.Helper()
	count := 0
	for _, expression := range expressionsToSearch {
		if expression == wantExpression {
			count++
		}
	}
	if count != wantCount {
		t.Fatalf("%s contains target pointer %d times, want %d", label, count, wantCount)
	}
}

func assertSameExpressionPointers(
	t testing.TB,
	label string,
	want []expressions.RelationalExpression,
	got []expressions.RelationalExpression,
) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s length = %d, want %d", label, len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s[%d] = %p, want %p", label, i, got[i], want[i])
		}
	}
}
