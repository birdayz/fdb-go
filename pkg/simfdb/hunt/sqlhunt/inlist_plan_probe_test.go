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

	// The shape is PINNED, not merely printed, because the pin is what makes the
	// diagnosis falsifiable: it was measured identical on master and on this
	// branch (`InUnion(IndexScan(IDX_CAT, [=]), bindings=1, ASC)` modulo the
	// `_current.` rendering), which is what rules the planner OUT as the cause of
	// the 4.4x and leaves per-execution setup as the explanation. If the planner
	// ever stops choosing an InUnion here, that conclusion needs re-taking and
	// this fails rather than going quietly stale.
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
				"moves the IN-list regression from per-execution setup to a planner choice.",
				probe.query, plan, probe.want)
		}
	}
}
