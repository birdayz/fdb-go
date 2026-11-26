package rowdiff

import (
	"fmt"
	"os"
	"strconv"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/relational/core/embedded"
)

// TestStatsInvariant_PurePlannerSweep is the STATISTICS axis (RFC-184 W1): it
// re-plans the generative corpus under REAL table statistics — the actual
// generated row count per case, versus the planner's default LeafScanCardinality
// (1e6) — and holds the cost + ordering invariants over the stats-driven plan.
//
// A stats-driven plan CHANGE that stays row-correct is fine and expected (the
// whole point of statistics); a plan that VIOLATES an invariant, or that fails
// to plan at all when the default did (statistics change only the winner's cost,
// never WHETHER a plan exists), is a finding. Row correctness of the DEFAULT-
// stats plan is proven separately by the FDB RunCase row-diff (run.go); this
// sweep adds the axis that the same invariants must survive real statistics.
//
// No FDB: PlanPhysicalForTest accepts the StatisticsProvider directly. The
// generator's single-table schema carries no persisted record count, so the SQL
// driver's own path plans under DEFAULT stats — a real MapStatistics here is the
// only way to reach the stats-driven plan surface without an engine change.
func TestStatsInvariant_PurePlannerSweep(t *testing.T) {
	t.Parallel()
	seeds := uint64(costSweepDefaultSeeds)
	if s := os.Getenv("ROWDIFF_COST_SWEEP_SEEDS"); s != "" {
		if n, err := strconv.ParseUint(s, 10, 64); err == nil && n > 0 {
			seeds = n
		}
	}

	var checked, violations int
	var familyChanged, explainChanged, plannabilityDiverged int
	var samples, planDivSamples []string

	type seedResult struct {
		checked, familyChanged, explainChanged int
		violations, planDiverged               []string
	}
	swept, release := defaultSweep(seeds)
	defer release()
	sweepSeeds(seeds, func(seed uint64) seedResult {
		var r seedResult
		sc := swept[seed-1]
		c, ddl := sc.c, sc.ddl
		// Representative cardinality: the case's actual row count (20..120),
		// four to five orders of magnitude below the default LeafScanCardinality,
		// so the cost model genuinely re-decides. Both PerType (keyed by the
		// generated table name) and Fallback are set so the lookup hits
		// regardless of the record-type name the planner resolves.
		card := float64(len(c.Rows))
		stats := properties.MapStatistics{
			PerType:  map[string]float64{c.Table.Name: card},
			Fallback: card,
		}
		for _, sp := range sc.plans {
			q, sqlText := sp.q, sp.sql
			planStats, errS := embedded.PlanPhysicalForTest(sqlText, ddl, stats)
			planDflt, errD := sp.plan, sp.err

			// Plannability parity: statistics change cost, never whether a
			// plan exists. A divergence is a finding.
			if (errS == nil) != (errD == nil) {
				r.planDiverged = append(r.planDiverged, fmt.Sprintf("%s (stats err=%v, default err=%v)", sqlText, errS, errD))
				continue
			}
			if errS != nil {
				continue // both errored — the row harness's concern, not this check's
			}
			r.checked++

			// The stats-driven plan must satisfy every cost/ordering
			// invariant a correct plan can never violate.
			for _, v := range checkPlanCost(planStats, q) {
				r.violations = append(r.violations, fmt.Sprintf("%s [cost: %s]", sqlText, v))
			}
			for _, v := range checkPlanOrdering(planStats, q) {
				r.violations = append(r.violations, fmt.Sprintf("%s [ordering: %s]", sqlText, v))
			}

			// Reach telemetry: proof the stats path actually re-decides.
			if !sameStringSet(classifyPlan(planStats), classifyPlan(planDflt)) {
				r.familyChanged++
			}
			if planStats.Explain() != planDflt.Explain() {
				r.explainChanged++
			}
		}
		return r
	}, func(seed uint64, r seedResult) {
		checked += r.checked
		familyChanged += r.familyChanged
		explainChanged += r.explainChanged
		for _, v := range r.violations {
			violations++
			if len(samples) < 10 {
				samples = append(samples, fmt.Sprintf("seed %d: %s", seed, v))
			}
		}
		for _, v := range r.planDiverged {
			plannabilityDiverged++
			if len(planDivSamples) < 10 {
				planDivSamples = append(planDivSamples, fmt.Sprintf("seed %d: %s", seed, v))
			}
		}
	})

	t.Logf("stats-invariant sweep: %d plans checked across %d seeds; plan family changed under stats=%d, plan shape changed=%d",
		checked, seeds, familyChanged, explainChanged)
	if checked == 0 {
		t.Fatal("planned zero queries — the sweep is not exercising the planner")
	}
	if plannabilityDiverged != 0 {
		for _, s := range planDivSamples {
			t.Errorf("plannability diverged under stats: %s", s)
		}
		t.Fatalf("%d queries changed plannability under real statistics — stats must never change whether a query plans", plannabilityDiverged)
	}
	if violations != 0 {
		for _, s := range samples {
			t.Errorf("stats-plan invariant violation: %s", s)
		}
		t.Fatalf("%d cost/ordering violations on stats-driven plans across %d plans — a correct engine must produce none", violations, checked)
	}
	// Wiring sanity: with a 4-5 order-of-magnitude cardinality swing across
	// thousands of plans, the cost model MUST re-decide at least one winner. A
	// flat zero means the StatisticsProvider is not reaching the planner (a
	// broken test), not that statistics are a no-op.
	if explainChanged == 0 {
		t.Fatal("no plan changed shape under real statistics across the whole sweep — the StatisticsProvider is not reaching the planner")
	}
}

// sameStringSet reports whether two already-sorted string slices are equal.
// classifyPlan returns a sorted slice, so a direct compare is a set compare.
func sameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
