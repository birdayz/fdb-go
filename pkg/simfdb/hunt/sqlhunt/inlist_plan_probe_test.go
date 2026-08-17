package sqlhunt

import (
	"context"
	"strings"
	"testing"
)

// TestInListPlanShape prints the plan the stress suite's IN-list query gets.
//
// That query — 46 rows out of 1M — regressed 4.37x on real FDB while every
// per-row fix left it untouched, which is what says its cost is not per-row.
// It is PLANNING cost: see the correction on the pin below.
// The corpus shows the mechanism on a neighbouring shape: a plain
// `PredicatesFilter(Scan)` became `InUnion(PredicatesFilter(Scan), bindings=1)`,
// and an InUnion re-executes its inner plan ONCE PER BINDING. Five IN values
// then buy five scans.
//
// The harness schema (t(id BIGINT, cat BIGINT, val BIGINT) with idx_cat,
// idx_val) is the stress schema's shape: an indexed non-key column carrying the
// IN list, ordered by the primary key.
func TestInListPlanShape(t *testing.T) {
	t.Parallel()
	h, err := qcNewHarness(910001)
	if err != nil {
		t.Fatalf("harness: %v", err)
	}
	defer h.close()

	ctx := context.Background()
	if _, err := h.db.ExecContext(ctx,
		"INSERT INTO t VALUES (1,0,10),(2,1,20),(3,2,30),(4,3,40),(5,4,50),(6,9,60)"); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// The shape is PINNED, not merely printed, so the diagnosis stays falsifiable.
	// It was measured identical on master and on this branch
	// (`InUnion(IndexScan(IDX_CAT, [=]), bindings=1, ASC)` modulo the `_current.`
	// rendering).
	//
	// That identity once read as "the planner is ruled out, so the cost is
	// per-execution setup". THAT READING IS WRONG and the correction is kept here
	// because the wrong version is the one a reader would otherwise inherit: an
	// identical CHOSEN PLAN says nothing about the cost of CHOOSING it.
	//
	// Reproduced, both trees, simulator equalized (master carrying this branch's
	// two pkg/simfdb allocation fixes so the comparison is of the ENGINE), 60
	// executions of one query via BenchmarkInListExecution:
	//
	//	master 5.4-6.6 ms/op   this branch 39.5 ms/op   (~6-7x)
	//
	// and under pprof the branch's timed loop is ~100% `cascadesGenerator.Plan`,
	// with `Planner.plan` at 2.56s of samples against master's 0.44s. Planning is
	// on the timed path at all because ResetSession invalidates the plan cache and
	// database/sql calls it on every pooled-connection reuse — so a repeated
	// identical query re-plans every time, on BOTH trees. That last fact is its
	// own defect and is not this branch's.
	//
	// WHERE inside planning is NOT established. Call-count instrumentation
	// pointed at data-access matching, but two probes in that attempt were
	// mis-attributed (one grep truncated, one edit landed in a neighbouring
	// function), so the counts are withdrawn rather than recorded — the timings
	// above are re-runnable and the counts were not.
	//
	// If the planner ever stops choosing an InUnion here, the comparison above
	// needs re-taking and this fails rather than going quietly stale.
	for _, probe := range []struct {
		query string
		want  string
	}{
		{"SELECT id, val FROM t WHERE cat IN (0, 1, 2, 3, 4) ORDER BY id", "InUnion"},
		{"SELECT id, val FROM t WHERE cat IN (0, 1, 2, 3, 4)", "InJoin"},
	} {
		var plan string
		if err := h.db.QueryRowContext(ctx, "EXPLAIN "+probe.query).Scan(&plan); err != nil {
			t.Fatalf("EXPLAIN %q: %v", probe.query, err)
		}
		t.Logf("INPLAN | %s\n    => %s", probe.query, plan)
		if !strings.Contains(plan, probe.want) {
			t.Errorf("IN-list plan for %q is %q, want a %s.\n"+
				"  The per-binding re-execution shape is what makes a 46-row query cost "+
				"5 executions, and it was measured IDENTICAL on master — so a change here "+
				"moves the IN-list regression from planning COST to a planner CHOICE.",
				probe.query, plan, probe.want)
		}
	}
}
