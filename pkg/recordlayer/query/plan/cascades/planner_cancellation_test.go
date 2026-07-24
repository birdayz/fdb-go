package cascades

import (
	"context"
	"errors"
	"testing"
	"time"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

type plannerCancellationTwoBindingMatcher struct{}

func (*plannerCancellationTwoBindingMatcher) RootType() string { return "any" }

func (*plannerCancellationTwoBindingMatcher) BindMatches(
	outer *matching.PlannerBindings,
	in any,
) []*matching.PlannerBindings {
	if _, ok := in.(expressions.RelationalExpression); !ok {
		return nil
	}
	return []*matching.PlannerBindings{outer, outer}
}

type plannerCancellationRule struct {
	matcher matching.BindingMatcher
	cancel  context.CancelCauseFunc
	cause   error
	calls   int
	runCtx  context.Context
	planner *Planner
	taskErr error
}

func newPlannerCancellationRule(cancel context.CancelCauseFunc, cause error) *plannerCancellationRule {
	return &plannerCancellationRule{
		matcher: &plannerCancellationTwoBindingMatcher{},
		cancel:  cancel,
		cause:   cause,
	}
}

func (r *plannerCancellationRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *plannerCancellationRule) OnMatch(call *ExpressionRuleCall) {
	r.calls++
	r.runCtx = call.RunContext
	if r.planner != nil && r.taskErr != nil {
		r.planner.capErr = r.taskErr
	}
	r.cancel(r.cause)
}

type plannerCancellationExtractionExpr struct {
	plan   plans.RecordQueryPlan
	cancel context.CancelCauseFunc
	cause  error
	calls  int
}

func (e *plannerCancellationExtractionExpr) GetResultValue() values.Value {
	return values.NewNullValue(values.UnknownType)
}

func (*plannerCancellationExtractionExpr) GetQuantifiers() []expressions.Quantifier {
	return nil
}

func (*plannerCancellationExtractionExpr) CanCorrelate() bool  { return false }
func (*plannerCancellationExtractionExpr) ChildrenAsSet() bool { return false }

func (*plannerCancellationExtractionExpr) HashCodeWithoutChildren() uint64 {
	return 0x637137
}

func (*plannerCancellationExtractionExpr) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return nil
}

func (e *plannerCancellationExtractionExpr) EqualsWithoutChildren(
	other expressions.RelationalExpression,
	_ *expressions.AliasMap,
) bool {
	_, ok := other.(*plannerCancellationExtractionExpr)
	return ok
}

func (e *plannerCancellationExtractionExpr) WithQuantifiers(
	_ []expressions.Quantifier,
) expressions.RelationalExpression {
	return e
}

func (e *plannerCancellationExtractionExpr) GetRecordQueryPlan() plans.RecordQueryPlan {
	return e.plan
}

func (e *plannerCancellationExtractionExpr) WithChildren(
	_ []expressions.Quantifier,
) (expressions.RelationalExpression, error) {
	e.calls++
	e.cancel(e.cause)
	return e, nil
}

func TestPlanner_PreCanceledContextIsZeroWorkAndConsumesPlanner(t *testing.T) {
	t.Parallel()

	cause := errors.New("pre-canceled planning")
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(cause)

	p := NewPlanner(DefaultExpressionRules(), nil)
	ref := plannerLifecycleTestRef()
	initialStage := ref.Stage()
	initialMembers := len(ref.AllMembers())

	plan, tasks, err := p.PlanWithContext(ctx, ref)
	if plan != nil || tasks != 0 {
		t.Fatalf("pre-canceled PlanWithContext = (%T, %d), want (nil, 0)", plan, tasks)
	}
	if !errors.Is(err, context.Canceled) || !errors.Is(err, cause) {
		t.Fatalf("pre-canceled err=%v, want context.Canceled and cause", err)
	}
	if p.Memo() != nil {
		t.Fatal("pre-canceled planning constructed a Memo")
	}
	if ref.ID() != 0 || ref.Stage() != initialStage || len(ref.AllMembers()) != initialMembers {
		t.Fatalf("pre-canceled planning mutated root: id=%d stage=%v members=%d",
			ref.ID(), ref.Stage(), len(ref.AllMembers()))
	}

	retryRef := plannerLifecycleTestRef()
	retryPlan, retryTasks, retryErr := p.PlanWithContext(context.Background(), retryRef)
	if retryPlan != nil || retryTasks != 0 || !errors.Is(retryErr, ErrPlannerAlreadyUsed) {
		t.Fatalf("retry = (%T, %d, %v), want (nil, 0, ErrPlannerAlreadyUsed)",
			retryPlan, retryTasks, retryErr)
	}
	if retryRef.ID() != 0 {
		t.Fatalf("rejected retry root ID=%d, want 0", retryRef.ID())
	}
}

func TestPlanner_ExpiredDeadlinePreservesClassificationAndCause(t *testing.T) {
	t.Parallel()

	cause := errors.New("planning deadline cause")
	ctx, cancel := context.WithDeadlineCause(context.Background(), time.Unix(1, 0), cause)
	defer cancel()

	p := NewPlanner(DefaultExpressionRules(), nil)
	plan, tasks, err := p.PlanWithContext(ctx, plannerLifecycleTestRef())
	if plan != nil || tasks != 0 {
		t.Fatalf("expired-deadline plan = (%T, %d), want (nil, 0)", plan, tasks)
	}
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, cause) {
		t.Fatalf("deadline err=%v, want context.DeadlineExceeded and cause", err)
	}
	if p.Memo() != nil {
		t.Fatal("expired deadline constructed a Memo")
	}
}

func TestPlanner_NilContextConsumesPlannerButCanceledNilRootDoesNot(t *testing.T) {
	t.Parallel()

	p := NewPlanner(DefaultExpressionRules(), nil)
	ref := plannerLifecycleTestRef()
	// Pass a typed nil deliberately to pin the public API's programmer-error
	// contract without triggering staticcheck's direct nil-context diagnostic.
	var nilCtx context.Context
	plan, tasks, err := p.PlanWithContext(nilCtx, ref)
	if plan != nil || tasks != 0 || !errors.Is(err, ErrPlannerNilContext) {
		t.Fatalf("nil-context plan = (%T, %d, %v), want (nil, 0, ErrPlannerNilContext)",
			plan, tasks, err)
	}
	if p.Memo() != nil || ref.ID() != 0 {
		t.Fatal("nil-context plan mutated run state before rejecting")
	}
	if _, _, retryErr := p.PlanWithContext(context.Background(), plannerLifecycleTestRef()); !errors.Is(retryErr, ErrPlannerAlreadyUsed) {
		t.Fatalf("retry after nil context err=%v, want ErrPlannerAlreadyUsed", retryErr)
	}

	nilRootPlanner := NewPlanner(DefaultExpressionRules(), nil)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if got, gotTasks, gotErr := nilRootPlanner.PlanWithContext(canceled, nil); got != nil || gotTasks != 0 || gotErr != nil {
		t.Fatalf("canceled nil-root plan = (%T, %d, %v), want no-op", got, gotTasks, gotErr)
	}
	if got, gotTasks, gotErr := nilRootPlanner.Plan(plannerLifecycleTestRef()); got == nil || gotTasks == 0 || gotErr != nil {
		t.Fatalf("Plan after canceled nil-root = (%T, %d, %v), want success", got, gotTasks, gotErr)
	}
}

func TestPlanner_MidTaskCancellationStopsRemainingBindings(t *testing.T) {
	t.Parallel()

	cause := errors.New("cancel from first binding")
	ctx, cancel := context.WithCancelCause(context.Background())
	rule := newPlannerCancellationRule(cancel, cause)
	p := NewPlanner([]ExpressionRule{rule}, nil)
	ref := plannerLifecycleTestRef()

	plan, tasks, err := p.PlanWithContext(ctx, ref)
	if plan != nil {
		t.Fatalf("mid-task cancellation returned %T, want nil", plan)
	}
	if !errors.Is(err, context.Canceled) || !errors.Is(err, cause) {
		t.Fatalf("mid-task err=%v, want context.Canceled and cause", err)
	}
	if rule.calls != 1 {
		t.Fatalf("rule calls=%d, want 1 of two bindings", rule.calls)
	}
	if rule.runCtx != ctx {
		t.Fatal("rule did not receive the exact run-scoped context")
	}
	if tasks != 4 || tasks != p.tasksRun {
		t.Fatalf("mid-task cancellation tasks=(returned %d, recorded %d), want (4, 4)",
			tasks, p.tasksRun)
	}
	if len(ref.FinalMembers()) != 0 {
		t.Fatalf("Finalize task ran after cancellation: %d finals", len(ref.FinalMembers()))
	}
	if len(p.stack) != 4 {
		t.Fatalf("residual stack length=%d, want 4 later tasks undrained", len(p.stack))
	}
	if p.Memo() == nil || p.Memo().reExplore != nil {
		t.Fatal("mid-task cancellation did not detach the Memo scheduler")
	}

	stackLen := len(p.stack)
	tasksRun := p.tasksRun
	retryRef := plannerLifecycleTestRef()
	retryPlan, retryTasks, retryErr := p.PlanWithContext(context.Background(), retryRef)
	if retryPlan != nil || retryTasks != 0 || !errors.Is(retryErr, ErrPlannerAlreadyUsed) {
		t.Fatalf("retry = (%T, %d, %v), want (nil, 0, ErrPlannerAlreadyUsed)",
			retryPlan, retryTasks, retryErr)
	}
	if len(p.stack) != stackLen || p.tasksRun != tasksRun || retryRef.ID() != 0 {
		t.Fatal("rejected retry mutated residual canceled-run state")
	}
}

func TestPlanner_ExtractionCancellationWinsAfterSuccessfulRebuild(t *testing.T) {
	t.Parallel()

	cause := errors.New("cancel during extraction rebuild")
	ctx, cancel := context.WithCancelCause(context.Background())
	expr := &plannerCancellationExtractionExpr{
		plan:   plans.NewRecordQueryValuesPlan(nil),
		cancel: cancel,
		cause:  cause,
	}
	p := NewPlanner(nil, nil)

	plan, tasks, err := p.PlanWithContext(ctx, expressions.InitialOf(expr))
	if plan != nil {
		t.Fatalf("extraction cancellation returned %T, want nil", plan)
	}
	if !errors.Is(err, context.Canceled) || !errors.Is(err, cause) {
		t.Fatalf("extraction err=%v, want context.Canceled and cause", err)
	}
	if expr.calls != 1 {
		t.Fatalf("WithChildren calls=%d, want 1", expr.calls)
	}
	if tasks == 0 || len(p.stack) != 0 {
		t.Fatalf("extraction fixture tasks=%d residual stack=%d, want drained task phase", tasks, len(p.stack))
	}
	if p.Memo() == nil || p.Memo().reExplore != nil {
		t.Fatal("extraction cancellation did not detach the Memo scheduler")
	}
}

func TestPlanner_TaskErrorTakesPrecedenceOverConcurrentCancellation(t *testing.T) {
	t.Parallel()

	cancelCause := errors.New("simultaneous cancellation")
	taskErr := errors.New("task invariant failed")
	ctx, cancel := context.WithCancelCause(context.Background())
	rule := newPlannerCancellationRule(cancel, cancelCause)
	p := NewPlanner([]ExpressionRule{rule}, nil)
	rule.planner = p
	rule.taskErr = taskErr

	plan, tasks, err := p.PlanWithContext(ctx, plannerLifecycleTestRef())
	if plan != nil || tasks == 0 {
		t.Fatalf("task error result = (%T, %d), want nil plan with completed task", plan, tasks)
	}
	if !errors.Is(err, taskErr) {
		t.Fatalf("err=%v, want task error precedence", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("task error was masked/combined with cancellation: %v", err)
	}
	if p.Memo() == nil || p.Memo().reExplore != nil {
		t.Fatal("task-error exit did not detach the Memo scheduler")
	}
}
