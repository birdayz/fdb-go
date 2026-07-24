package cascades

import (
	"context"
	"errors"
	"testing"
)

// The compiler enforces that production code cannot reach the context-less
// Plan, so what remains testable is the contract that survives that move: the
// test-only Plan is not a second lifecycle but a literal delegation to the
// same one.

func TestPlanner_PlanIsBackgroundContextPlanWithContext(t *testing.T) {
	t.Parallel()

	// Both entry points must produce the same plan for the same input, run the
	// same number of tasks, and consume the same single-use lifecycle. A second
	// entry point that diverged on any of those would be a distinct code path,
	// which is exactly what routing everything through plan forbids.
	viaPlan := NewPlanner(DefaultExpressionRules(), nil)
	planExpr, planTasks, err := viaPlan.Plan(plannerLifecycleTestRef())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	viaContext := NewPlanner(DefaultExpressionRules(), nil)
	ctxExpr, ctxTasks, err := viaContext.PlanWithContext(context.Background(), plannerLifecycleTestRef())
	if err != nil {
		t.Fatalf("PlanWithContext: %v", err)
	}

	if planExpr == nil || ctxExpr == nil {
		t.Fatalf("entry points returned (%T, %T), want plans from both", planExpr, ctxExpr)
	}
	if !planExpr.EqualsWithoutChildren(ctxExpr, nil) {
		t.Fatalf("Plan produced %T, PlanWithContext produced %T", planExpr, ctxExpr)
	}
	if planTasks != ctxTasks {
		t.Fatalf("Plan ran %d tasks, PlanWithContext ran %d", planTasks, ctxTasks)
	}

	// Single-use consumption is claimed by the shared lifecycle, so it must be
	// symmetric across the entry points in both directions.
	if _, _, retryErr := viaPlan.PlanWithContext(context.Background(), plannerLifecycleTestRef()); !errors.Is(retryErr, ErrPlannerAlreadyUsed) {
		t.Fatalf("PlanWithContext after Plan err=%v, want ErrPlannerAlreadyUsed", retryErr)
	}
	if _, _, retryErr := viaContext.Plan(plannerLifecycleTestRef()); !errors.Is(retryErr, ErrPlannerAlreadyUsed) {
		t.Fatalf("Plan after PlanWithContext err=%v, want ErrPlannerAlreadyUsed", retryErr)
	}

	// A nil root is a zero-work no-op on both, and leaves the lifecycle unclaimed.
	nilRoot := NewPlanner(DefaultExpressionRules(), nil)
	if got, gotTasks, gotErr := nilRoot.Plan(nil); got != nil || gotTasks != 0 || gotErr != nil {
		t.Fatalf("Plan(nil) = (%T, %d, %v), want (nil, 0, nil)", got, gotTasks, gotErr)
	}
	if got, gotTasks, gotErr := nilRoot.PlanWithContext(context.Background(), nil); got != nil || gotTasks != 0 || gotErr != nil {
		t.Fatalf("PlanWithContext(nil root) = (%T, %d, %v), want (nil, 0, nil)", got, gotTasks, gotErr)
	}
	if got, gotTasks, gotErr := nilRoot.Plan(plannerLifecycleTestRef()); got == nil || gotTasks == 0 || gotErr != nil {
		t.Fatalf("Plan after nil roots = (%T, %d, %v), want a real planning run", got, gotTasks, gotErr)
	}
}
