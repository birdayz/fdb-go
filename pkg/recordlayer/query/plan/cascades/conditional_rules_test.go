package cascades

import (
	"context"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

type countingImplementationRule struct {
	matcher matching.BindingMatcher
	yield   expressions.RelationalExpression
	pruned  bool
	calls   int
}

func (r *countingImplementationRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *countingImplementationRule) OnMatch(call *ImplementationRuleCall) {
	r.calls++
	if r.yield != nil {
		call.Yield(r.yield)
	}
}

func (r *countingImplementationRule) OnlyOnPrunedInputs() bool { return r.pruned }

type countingExpressionRule struct {
	matcher matching.BindingMatcher
	yield   expressions.RelationalExpression
	calls   int
}

func (r *countingExpressionRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *countingExpressionRule) OnMatch(call *ExpressionRuleCall) {
	r.calls++
	if r.yield != nil {
		call.Yield(r.yield)
	}
}

func drainTasks(ctx context.Context, p *Planner) {
	for len(p.stack) > 0 {
		p.pop().Run(ctx, p)
	}
}

// Java ConditionalTransformExpression: a conditional rule's next rule runs only
// when the one before it made no progress on the (group, expression) pair.
func TestConditionalImplementationRule_NextRuleRunsOnlyWithoutProgress(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	matcher := func() matching.BindingMatcher {
		return NewExpressionMatcher[*expressions.FullUnorderedScanExpression]("conditional")
	}
	plan, err := plans.NewRecordQueryScanPlan([]string{"COND"}, values.NotNullLong, false)
	plan = mustConstruct(t, plan, err)

	for _, tc := range []struct {
		name       string
		firstYield expressions.RelationalExpression
		wantSecond int
	}{
		{"first makes progress", plan, 0},
		{"first makes none", nil, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			first := &countingImplementationRule{matcher: matcher(), yield: tc.firstYield}
			second := &countingImplementationRule{matcher: matcher()}
			scan := fixtureScan("COND")
			ref := expressions.InitialOf(scan)
			p := NewPlanner(nil, nil)
			p.constraintMap = NewConstraintMap()
			p.push(&TransformImplTask{Phase: PhaseRewriting, Ref: ref, Expr: scan, Rule: newConditionalImplementationRule(first, second)})
			drainTasks(ctx, p)
			if p.capErr != nil {
				t.Fatal(p.capErr)
			}
			if first.calls != 1 || second.calls != tc.wantSecond {
				t.Fatalf("calls first=%d second=%d, want 1 and %d", first.calls, second.calls, tc.wantSecond)
			}
		})
	}
}

func TestConditionalExpressionRule_NextRuleRunsOnlyWithoutProgress(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	matcher := func() matching.BindingMatcher {
		return NewExpressionMatcher[*expressions.FullUnorderedScanExpression]("conditional")
	}
	for _, tc := range []struct {
		name       string
		firstYield expressions.RelationalExpression
		wantSecond int
	}{
		{"first makes progress", fixtureScan("OTHER"), 0},
		{"first makes none", nil, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			first := &countingExpressionRule{matcher: matcher(), yield: tc.firstYield}
			second := &countingExpressionRule{matcher: matcher()}
			scan := fixtureScan("COND")
			ref := expressions.InitialOf(scan)
			p := NewPlanner(nil, nil)
			p.constraintMap = NewConstraintMap()
			task := &TransformExprTask{Phase: PhaseRewriting, Ref: ref, Expr: scan, Rule: newConditionalExpressionRule(first, second)}
			task.Run(ctx, p)
			// Run only the conditional's continuation, not the yield's exploration.
			for len(p.stack) > 0 {
				if next, ok := p.pop().(*TransformExprTask); ok && next.Rule == task.Rule {
					next.Run(ctx, p)
				}
			}
			if p.capErr != nil {
				t.Fatal(p.capErr)
			}
			if first.calls != 1 || second.calls != tc.wantSecond {
				t.Fatalf("calls first=%d second=%d, want 1 and %d", first.calls, second.calls, tc.wantSecond)
			}
		})
	}
}

// Java's OnPrunedInputsRule: exploration never schedules the rule; OptimizeInputs
// schedules it beneath the input groups' OptimizeGroup tasks.
func TestPrunedInputsRule_ScheduledOnlyByOptimizeInputs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rule := &countingImplementationRule{
		matcher: NewExpressionMatcher[*expressions.SelectExpression]("pruned"),
		pruned:  true,
	}
	childRef := expressions.InitialOf(fixtureScan("PRUNED"))
	q := expressions.ForEachQuantifier(childRef)
	flowed, err := q.RequireFlowedObjectValue()
	if err != nil {
		t.Fatal(err)
	}
	sel, err := expressions.NewSelectExpression(flowed, []expressions.Quantifier{q}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ref := expressions.InitialOf(sel)
	p := NewPlanner(nil, nil)
	p.rewritingImplRules = []ImplementationRule{rule}
	p.constraintMap = NewConstraintMap()

	(&ExploreExprTask{Phase: PhaseRewriting, Ref: ref, Expr: sel}).Run(ctx, p)
	for _, task := range p.stack {
		if transform, ok := task.(*TransformImplTask); ok && transform.Rule == rule {
			t.Fatal("exploration scheduled a pruned-inputs rule")
		}
	}
	p.stack = nil

	(&OptimizeInputsTask{Phase: PhaseRewriting, Ref: ref, Expr: sel}).Run(ctx, p)
	ruleAt, groupAt := -1, -1
	for i, task := range p.stack {
		switch task := task.(type) {
		case *TransformImplTask:
			if task.Rule == rule {
				ruleAt = i
			}
		case *OptimizeGroupTask:
			if task.Ref == childRef {
				groupAt = i
			}
		}
	}
	if ruleAt < 0 || groupAt < 0 || ruleAt > groupAt {
		t.Fatalf("OptimizeInputs must push the rule beneath the input's OptimizeGroup: rule at %d, group at %d", ruleAt, groupAt)
	}
}

// Disabling a rule by name reaches inside a conditional rule, as Java's
// isRuleEnabled filters each inner rule.
func TestEnabledImplementationRules_FilterConditionalInnerRules(t *testing.T) {
	t.Parallel()
	rules := RewritingImplementationRules()
	onlyPushDown := enabledImplementationRules(rules, map[string]struct{}{"SelectMergeRule": {}})
	cond, ok := onlyPushDown[0].(*conditionalImplementationRule)
	if !ok || len(cond.rules) != 1 {
		t.Fatalf("disabling SelectMergeRule: got %T", onlyPushDown[0])
	}
	if _, ok := cond.rules[0].(*PredicatePushDownRule); !ok {
		t.Fatalf("remaining inner rule %T, want PredicatePushDownRule", cond.rules[0])
	}
	none := enabledImplementationRules(rules, map[string]struct{}{"SelectMergeRule": {}, "PredicatePushDownRule": {}})
	if len(none) != 1 {
		t.Fatalf("disabling both inner rules must drop the conditional, got %d rules", len(none))
	}
	if _, ok := none[0].(*FinalizeExpressionsRule); !ok {
		t.Fatalf("got %T, want only FinalizeExpressionsRule", none[0])
	}
	explore := enabledExpressionRules(RewritingRules(), map[string]struct{}{"DecorrelateValuesRule": {}})
	econd, ok := explore[0].(*conditionalExpressionRule)
	if !ok || len(econd.rules) != 1 {
		t.Fatalf("disabling DecorrelateValuesRule: got %T", explore[0])
	}
	if _, ok := econd.rules[0].(*QueryPredicateSimplificationRule); !ok {
		t.Fatalf("remaining inner rule %T, want QueryPredicateSimplificationRule", econd.rules[0])
	}
}
