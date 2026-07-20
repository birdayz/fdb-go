package rowdiff

import (
	"fmt"
	"os"
	"strconv"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
	"fdb.dev/pkg/relational/core/embedded"
)

// TestCheckPlanCost_SpuriousSort proves the cost/ordering invariant both FIRES
// on a violation and stays quiet on every legitimate reason a plan may sort —
// the sensitivity net for the check itself (RFC-182 OQ-5, cost axis).
func TestCheckPlanCost_SpuriousSort(t *testing.T) {
	t.Parallel()
	scan := plans.NewRecordQueryScanPlan(nil, nil, false)
	sorted := plans.NewRecordQueryInMemorySortPlan(scan, nil)

	// A bare single-table SELECT with a WHERE and no ordering requirement.
	bare := Query{Where: leaf("A", predicates.ComparisonEquals, int64(1))}

	// FIRES: a no-ordering single-table query whose plan carries a sort.
	if got := checkPlanCost(sorted, bare); len(got) == 0 {
		t.Fatal("spurious sort over a no-ordering single-table query not flagged")
	}
	// CLEAN: the same query planned as a bare scan — no sort, no finding.
	if got := checkPlanCost(scan, bare); len(got) != 0 {
		t.Fatalf("bare scan flagged as spurious sort: %v", got)
	}
	// CLEAN: an ORDER BY makes the sort a legitimate ordering operator.
	if got := checkPlanCost(sorted, Query{OrderBy: []OrderKey{{Col: "A"}}}); len(got) != 0 {
		t.Fatalf("sort under an ORDER BY query flagged: %v", got)
	}
	// CLEAN: SELECT DISTINCT may dedup via a sort.
	if got := checkPlanCost(sorted, Query{Distinct: true}); len(got) != 0 {
		t.Fatalf("sort under a DISTINCT query flagged: %v", got)
	}
	// CLEAN: an aggregate may sort to group.
	if got := checkPlanCost(sorted, Query{Agg: &AggSpec{}}); len(got) != 0 {
		t.Fatalf("sort under an aggregate query flagged: %v", got)
	}
	// CLEAN: a set operation legitimately merge-sorts its index-scan inputs
	// into a common primary-key order even without a query ORDER BY (the OR
	// lowering). The sort lives UNDER the union, so the whole plan is excused.
	union := plans.NewRecordQueryUnorderedUnionPlan([]plans.RecordQueryPlan{sorted, scan})
	if got := checkPlanCost(union, bare); len(got) != 0 {
		t.Fatalf("sort beneath a set operation flagged: %v", got)
	}
}

// TestCostInvariant_PurePlannerSweep validates the spurious-sort invariant
// against the REAL planner over the generative corpus, with no FDB: every
// generated query is planned and checked, and a correct engine must produce
// zero violations. This is the permanent net — if a planner change ever starts
// emitting a redundant sort for a no-ordering single-table query, this test
// goes red without needing a container. A non-zero count prints the offending
// SQL for immediate triage.
func TestCostInvariant_PurePlannerSweep(t *testing.T) {
	t.Parallel()
	// Each seed drives a full Cascades planning pass per query×projection, so
	// the committed budget is modest (still thousands of plans). It is smaller
	// under the race detector (costSweepDefaultSeeds is build-tagged) so the
	// serial sweep stays within the test timeout. A wider validation run is
	// available via ROWDIFF_COST_SWEEP_SEEDS=N.
	seeds := uint64(costSweepDefaultSeeds)
	if s := os.Getenv("ROWDIFF_COST_SWEEP_SEEDS"); s != "" {
		if n, err := strconv.ParseUint(s, 10, 64); err == nil && n > 0 {
			seeds = n
		}
	}
	var checked, violations int
	var samples []string
	for seed := uint64(1); seed <= seeds; seed++ {
		c := Generate(seed)
		ddl := c.DDL()
		for _, q := range c.Queries {
			for _, proj := range c.ProjectionsFor(q) {
				sqlText := c.SQL(q, proj)
				plan, err := embedded.PlanPhysicalForTest(sqlText, ddl, nil)
				if err != nil {
					continue // plan errors are the row harness's concern, not this check's
				}
				checked++
				for _, v := range checkPlanCost(plan, q) {
					violations++
					if len(samples) < 10 {
						samples = append(samples, fmt.Sprintf("seed %d: %s [%s]", seed, sqlText, v))
					}
				}
			}
		}
	}
	t.Logf("cost-invariant sweep: %d plans checked across %d seeds", checked, seeds)
	if checked == 0 {
		t.Fatal("planned zero queries — the sweep is not exercising the planner")
	}
	if violations != 0 {
		for _, s := range samples {
			t.Errorf("cost/ordering violation: %s", s)
		}
		t.Fatalf("%d cost/ordering violations across %d plans — a correct engine must produce none", violations, checked)
	}
}
