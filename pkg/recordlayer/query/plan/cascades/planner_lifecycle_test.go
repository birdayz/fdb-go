package cascades

import (
	"errors"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

var errPlannerLifecycleExtraction = errors.New("planner lifecycle extraction failure")

// plannerLifecycleExtractionErrorExpr is a physical leaf whose extraction
// rebuilder fails. It exercises a post-task error exit, distinct from the
// task-cap exit covered below.
type plannerLifecycleExtractionErrorExpr struct {
	plan plans.RecordQueryPlan
}

func (e *plannerLifecycleExtractionErrorExpr) GetResultValue() values.Value {
	return values.NewNullValue(values.UnknownType)
}

func (*plannerLifecycleExtractionErrorExpr) GetQuantifiers() []expressions.Quantifier {
	return nil
}

func (*plannerLifecycleExtractionErrorExpr) CanCorrelate() bool  { return false }
func (*plannerLifecycleExtractionErrorExpr) ChildrenAsSet() bool { return false }
func (*plannerLifecycleExtractionErrorExpr) HashCodeWithoutChildren() uint64 {
	return 0x637136
}

func (*plannerLifecycleExtractionErrorExpr) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return nil
}

func (e *plannerLifecycleExtractionErrorExpr) EqualsWithoutChildren(
	other expressions.RelationalExpression,
	_ *expressions.AliasMap,
) bool {
	_, ok := other.(*plannerLifecycleExtractionErrorExpr)
	return ok
}

func (e *plannerLifecycleExtractionErrorExpr) WithQuantifiers(
	_ []expressions.Quantifier,
) expressions.RelationalExpression {
	return e
}

func (e *plannerLifecycleExtractionErrorExpr) GetRecordQueryPlan() plans.RecordQueryPlan {
	return e.plan
}

func (*plannerLifecycleExtractionErrorExpr) WithChildren(
	_ []expressions.Quantifier,
) (expressions.RelationalExpression, error) {
	return nil, errPlannerLifecycleExtraction
}

func plannerLifecycleTestRef() *expressions.Reference {
	return expressions.InitialOf(
		expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType),
	)
}

func TestPlanner_RejectsSecondPlanAfterSuccess(t *testing.T) {
	t.Parallel()

	p := NewPlanner(DefaultExpressionRules(), nil)
	firstRef := plannerLifecycleTestRef()
	firstPlan, firstTasks, err := p.Plan(firstRef)
	if err != nil {
		t.Fatalf("first Plan: %v", err)
	}
	if firstPlan == nil {
		t.Fatal("first Plan returned nil")
	}
	if firstTasks == 0 {
		t.Fatal("first Plan ran no tasks")
	}

	firstMemo := p.Memo()
	if firstMemo == nil || firstMemo.Root() != firstRef {
		t.Fatal("first Plan did not retain its root in the Memo")
	}
	firstStackLen := len(p.stack)
	secondRef := plannerLifecycleTestRef()
	secondRefStage := secondRef.Stage()
	if secondRef.ID() != 0 {
		t.Fatalf("fresh second root ID=%d, want 0", secondRef.ID())
	}
	secondPlan, secondTasks, err := p.Plan(secondRef)
	if !errors.Is(err, ErrPlannerAlreadyUsed) {
		t.Fatalf("second Plan err=%v, want ErrPlannerAlreadyUsed", err)
	}
	if secondPlan != nil {
		t.Fatalf("second Plan returned %T, want nil", secondPlan)
	}
	if secondTasks != 0 {
		t.Fatalf("second Plan reported %d tasks, want 0", secondTasks)
	}

	// Rejection happens before any per-run state or caller-owned Reference is
	// touched. In particular, a different root cannot be silently attached to
	// the first run's Memo.
	if p.tasksRun != firstTasks {
		t.Fatalf("second Plan changed cumulative tasks from %d to %d", firstTasks, p.tasksRun)
	}
	if p.Memo() != firstMemo {
		t.Fatal("second Plan replaced the first run's Memo")
	}
	if firstMemo != nil && firstMemo.reExplore != nil {
		t.Fatal("first run left its Memo scheduler attached after success")
	}
	if len(p.stack) != firstStackLen {
		t.Fatalf("second Plan changed stack length from %d to %d", firstStackLen, len(p.stack))
	}
	if secondRef.Stage() != secondRefStage {
		t.Fatalf("second root stage changed from %v to %v", secondRefStage, secondRef.Stage())
	}
	if firstMemo != nil && firstMemo.ContainsReference(secondRef) {
		t.Fatal("second root was inserted into the first run's Memo")
	}
	if secondRef.ID() != 0 {
		t.Fatalf("second Plan assigned rejected root ID=%d, want 0", secondRef.ID())
	}
}

func TestPlanner_RejectsRetryAfterCapError(t *testing.T) {
	t.Parallel()

	p := NewPlanner(DefaultExpressionRules(), nil).WithMaxTasks(1)
	firstRef := plannerLifecycleTestRef()
	firstPlan, firstTasks, err := p.Plan(firstRef)
	if !errors.Is(err, ErrPlannerCapHit) {
		t.Fatalf("first Plan err=%v, want ErrPlannerCapHit", err)
	}
	if firstPlan != nil {
		t.Fatalf("capped Plan returned %T, want nil", firstPlan)
	}
	if firstTasks != 1 {
		t.Fatalf("capped Plan ran %d tasks, want 1", firstTasks)
	}

	// Raising the cap must not turn the partially-drained task stack into a
	// resumable planner. Retrying requires a fresh Planner and a fresh root.
	firstMemo := p.Memo()
	if firstMemo == nil || firstMemo.Root() != firstRef {
		t.Fatal("capped Plan did not retain its root in the Memo")
	}
	firstStackLen := len(p.stack)
	if firstStackLen == 0 {
		t.Fatal("cap fixture did not leave a partially-drained task stack")
	}
	p.WithMaxTasks(100_000)
	retryRef := plannerLifecycleTestRef()
	retryRefStage := retryRef.Stage()
	if retryRef.ID() != 0 {
		t.Fatalf("fresh retry root ID=%d, want 0", retryRef.ID())
	}
	retryPlan, retryTasks, err := p.Plan(retryRef)
	if !errors.Is(err, ErrPlannerAlreadyUsed) {
		t.Fatalf("retry Plan err=%v, want ErrPlannerAlreadyUsed", err)
	}
	if retryPlan != nil {
		t.Fatalf("retry Plan returned %T, want nil", retryPlan)
	}
	if retryTasks != 0 {
		t.Fatalf("retry Plan reported %d tasks, want 0", retryTasks)
	}
	if p.tasksRun != firstTasks {
		t.Fatalf("retry changed cumulative tasks from %d to %d", firstTasks, p.tasksRun)
	}
	if p.Memo() != firstMemo {
		t.Fatal("retry replaced the capped run's Memo")
	}
	if firstMemo != nil && firstMemo.reExplore != nil {
		t.Fatal("capped run left its Memo scheduler attached")
	}
	if len(p.stack) != firstStackLen {
		t.Fatalf("retry changed residual stack length from %d to %d", firstStackLen, len(p.stack))
	}
	if retryRef.Stage() != retryRefStage {
		t.Fatalf("retry root stage changed from %v to %v", retryRefStage, retryRef.Stage())
	}
	if firstMemo != nil && firstMemo.ContainsReference(retryRef) {
		t.Fatal("retry root was inserted into the capped run's Memo")
	}
	if retryRef.ID() != 0 {
		t.Fatalf("retry assigned rejected root ID=%d, want 0", retryRef.ID())
	}
}

func TestPlanner_RejectsRetryAfterExtractionError(t *testing.T) {
	t.Parallel()

	bad := &plannerLifecycleExtractionErrorExpr{plan: plans.NewRecordQueryValuesPlan(nil)}
	p := NewPlanner(nil, nil)
	firstPlan, firstTasks, err := p.Plan(expressions.InitialOf(bad))
	if !errors.Is(err, errPlannerLifecycleExtraction) {
		t.Fatalf("first Plan err=%v, want extraction failure", err)
	}
	if firstPlan != nil {
		t.Fatalf("failed extraction returned %T, want nil", firstPlan)
	}
	if firstTasks == 0 {
		t.Fatal("failed extraction ran no planner tasks")
	}
	if len(p.stack) != 0 {
		t.Fatalf("extraction-error fixture left %d tasks, want a drained stack", len(p.stack))
	}

	firstMemo := p.Memo()
	if firstMemo == nil || firstMemo.Root() == nil {
		t.Fatal("failed Plan did not retain its root in the Memo")
	}
	retryRef := plannerLifecycleTestRef()
	retryStage := retryRef.Stage()
	if retryRef.ID() != 0 {
		t.Fatalf("fresh retry root ID=%d, want 0", retryRef.ID())
	}
	retryPlan, retryTasks, err := p.Plan(retryRef)
	if !errors.Is(err, ErrPlannerAlreadyUsed) {
		t.Fatalf("retry Plan err=%v, want ErrPlannerAlreadyUsed", err)
	}
	if retryPlan != nil || retryTasks != 0 {
		t.Fatalf("retry Plan = (%T, %d), want (nil, 0)", retryPlan, retryTasks)
	}
	if p.tasksRun != firstTasks {
		t.Fatalf("retry changed cumulative tasks from %d to %d", firstTasks, p.tasksRun)
	}
	if p.Memo() != firstMemo {
		t.Fatal("retry replaced the failed run's Memo")
	}
	if firstMemo != nil && firstMemo.reExplore != nil {
		t.Fatal("failed extraction left its Memo scheduler attached")
	}
	if retryRef.Stage() != retryStage {
		t.Fatalf("retry root stage changed from %v to %v", retryStage, retryRef.Stage())
	}
	if firstMemo != nil && firstMemo.ContainsReference(retryRef) {
		t.Fatal("retry root was inserted into the failed run's Memo")
	}
	if retryRef.ID() != 0 {
		t.Fatalf("retry assigned rejected root ID=%d, want 0", retryRef.ID())
	}
}

func TestPlanner_NilPlanDoesNotConsumeSingleUseLifecycle(t *testing.T) {
	t.Parallel()

	p := NewPlanner(DefaultExpressionRules(), nil)
	if plan, tasks, err := p.Plan(nil); err != nil || plan != nil || tasks != 0 {
		t.Fatalf("Plan(nil) = (%T, %d, %v), want (nil, 0, nil)", plan, tasks, err)
	}

	plan, tasks, err := p.Plan(plannerLifecycleTestRef())
	if err != nil {
		t.Fatalf("first non-nil Plan after Plan(nil): %v", err)
	}
	if plan == nil || tasks == 0 {
		t.Fatalf("first non-nil Plan after Plan(nil) = (%T, %d), want a plan and tasks", plan, tasks)
	}

	// Nil remains a no-op even after the one real run has consumed the Planner.
	if nilPlan, nilTasks, nilErr := p.Plan(nil); nilErr != nil || nilPlan != nil || nilTasks != 0 {
		t.Fatalf("second Plan(nil) = (%T, %d, %v), want (nil, 0, nil)", nilPlan, nilTasks, nilErr)
	}
}
