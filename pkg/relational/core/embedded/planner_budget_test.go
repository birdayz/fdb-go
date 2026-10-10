package embedded

import (
	"context"
	"errors"
	"testing"
	"time"

	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// sixTableChainSQL is a six-table FK chain over the star schema: the widest
// chain the default embedded task budget still plans, in seconds.
const sixTableChainSQL = "SELECT H.id, S1.id, S2.id, S3.id, S4.id, S5.id " +
	"FROM H, S1, S2, S3, S4, S5 " +
	"WHERE H.id = S1.hid AND S1.id = S2.hid AND S2.id = S3.hid AND S3.id = S4.hid AND S4.id = S5.hid"

// TestPlannerBudget_SixTableChain pins the six-table join's search size against
// the embedded task budget, and that the caller's deadline is the wall-clock
// budget: Java has none, so a Go caller bounds planning with its context.
func TestPlannerBudget_SixTableChain(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, canceledTasks, err := planWithOptionsContext(ctx, t, sixTableChainSQL, starJoinDDL, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("200ms deadline: err=%v after %d tasks, want context.DeadlineExceeded", err, canceledTasks)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("planning stopped %v after a 200ms deadline; the task loop must observe it", elapsed)
	}

	plan, tasks, err := planWithOptions(t, sixTableChainSQL, starJoinDDL, nil)
	if err != nil {
		t.Fatalf("six-table chain: %v (tasks=%d), want a plan within %d tasks", err, tasks, embeddedPlannerMaxTasks)
	}
	if canceledTasks >= tasks {
		t.Fatalf("canceled run executed %d tasks, the complete one %d", canceledTasks, tasks)
	}
	tables := make(map[string]bool)
	var visit func(plans.RecordQueryPlan)
	visit = func(node plans.RecordQueryPlan) {
		if leaf, ok := node.(interface{ GetRecordTypes() []string }); ok && len(node.GetChildren()) == 0 {
			for _, name := range leaf.GetRecordTypes() {
				tables[name] = true
			}
		}
		for _, child := range node.GetChildren() {
			visit(child)
		}
	}
	visit(plan)
	for _, name := range []string{"H", "S1", "S2", "S3", "S4", "S5"} {
		if !tables[name] {
			t.Fatalf("six-table chain lost table %s: %s", name, plan.Explain())
		}
	}
	// Growth alarm toward the 150k ceiling, collapse alarm for a pruned search.
	const observedTasks = 110_701
	tol := observedTasks / 50
	if tasks < observedTasks-tol || tasks > observedTasks+tol {
		t.Errorf("six-table chain tasks=%d, want %d +/-2%% ([%d,%d]) of the %d budget. Above the band "+
			"the chain is climbing toward a cap error Java would not raise; below it, re-baseline "+
			"only after checking the chain still joins all six tables.",
			tasks, observedTasks, observedTasks-tol, observedTasks+tol, embeddedPlannerMaxTasks)
	}
}

func BenchmarkPlanSixTableChain(b *testing.B) {
	for b.Loop() {
		_, tasks, err := planWithOptions(b, sixTableChainSQL, starJoinDDL, nil)
		if err != nil {
			b.Fatalf("six-table chain: %v (tasks=%d)", err, tasks)
		}
		b.ReportMetric(float64(tasks), "tasks/op")
	}
}
