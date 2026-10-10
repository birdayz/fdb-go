package embedded

import (
	"context"
	"errors"
	"testing"
	"time"

	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// sixTableChainSQL is the six-table FK chain over the star schema whose
// planning time this file measures; fiveTableChainSQL is its cheaper prefix.
const (
	sixTableChainSQL = "SELECT H.id, S1.id, S2.id, S3.id, S4.id, S5.id " +
		"FROM H, S1, S2, S3, S4, S5 " +
		"WHERE H.id = S1.hid AND S1.id = S2.hid AND S2.id = S3.hid AND S3.id = S4.hid AND S4.id = S5.hid"
	fiveTableChainSQL = "SELECT H.id, S1.id, S2.id, S3.id, S4.id " +
		"FROM H, S1, S2, S3, S4 " +
		"WHERE H.id = S1.hid AND S1.id = S2.hid AND S2.id = S3.hid AND S3.id = S4.hid"
)

// TestPlannerBudget_ChainJoin pins the chain's search size, and that with no
// task budget (Java's default) the caller's deadline still bounds planning.
func TestPlannerBudget_ChainJoin(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, canceledTasks, err := planWithOptionsContext(ctx, t, sixTableChainSQL, starJoinDDL, nil)
	if !errors.Is(err, context.DeadlineExceeded) || canceledTasks == 0 {
		t.Fatalf("200ms deadline: err=%v after %d tasks, want context.DeadlineExceeded from the task loop",
			err, canceledTasks)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("planning stopped %v after a 200ms deadline; the task loop must observe it", elapsed)
	}

	plan, tasks, err := planWithOptions(t, fiveTableChainSQL, starJoinDDL, nil)
	if err != nil {
		t.Fatalf("five-table chain: %v (tasks=%d)", err, tasks)
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
	for _, name := range []string{"H", "S1", "S2", "S3", "S4"} {
		if !tables[name] {
			t.Fatalf("five-table chain lost table %s: %s", name, plan.Explain())
		}
	}
	const observedTasks = 13_341
	tol := observedTasks / 50
	if tasks < observedTasks-tol || tasks > observedTasks+tol {
		t.Errorf("five-table chain tasks=%d, want %d +/-2%% ([%d,%d]). Above the band the search "+
			"grew; below it, re-baseline only after checking the chain still joins all five tables.",
			tasks, observedTasks, observedTasks-tol, observedTasks+tol)
	}
}

func BenchmarkPlanChainJoin(b *testing.B) {
	for _, tc := range []struct{ name, sql string }{{"five", fiveTableChainSQL}, {"six", sixTableChainSQL}} {
		b.Run(tc.name, func(b *testing.B) {
			for b.Loop() {
				_, tasks, err := planWithOptions(b, tc.sql, starJoinDDL, nil)
				if err != nil {
					b.Fatalf("%s-table chain: %v (tasks=%d)", tc.name, err, tasks)
				}
				b.ReportMetric(float64(tasks), "tasks/op")
			}
		})
	}
}
