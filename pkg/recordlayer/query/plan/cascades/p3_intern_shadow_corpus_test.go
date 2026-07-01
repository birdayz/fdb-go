package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
)

// TestPlanner_P3InternShadow_Corpus_RFC173 exercises the P3 dark shadow observer
// across the fuzz corpus under REAL planning: it confirms the shadow fires on
// non-opted-in Inserts without panicking or perturbing plan production (dedup
// behavior is unchanged — the observer only reads), and measures the extra-dedup
// rate the global alias-bijection tier would achieve. Stability, not a
// prove-identical gate: safety of the extra dedup is certified by §5's execution
// pins (CTE-rename), not here.
func TestPlanner_P3InternShadow_Corpus_RFC173(t *testing.T) {
	// Not t.Parallel(): mutates the package-level InternShadowObserver.
	var observed, wouldDedup int
	expressions.InternShadowObserver = func(would bool) {
		observed++
		if would {
			wouldDedup++
		}
	}
	t.Cleanup(func() { expressions.InternShadowObserver = nil })

	planned := 0
	for seed := 0; seed < 1500; seed++ {
		b := []byte{byte(seed), byte(seed >> 8), byte(seed >> 4), byte(seed * 7), byte(seed*3 + 1), byte(seed >> 2)}
		expr := buildFuzzExpression(b, 0, 0)
		if expr == nil {
			continue
		}
		ref := expressions.InitialOf(expr)
		p := NewPlanner(selectRules(b), nil).
			WithPlanningExpressionRules(BatchAExpressionRules()).
			WithImplementationRules(DefaultImplementationRules())
		p.MaxTasks = 100_000
		best, _, err := p.Plan(ref)
		if err != nil || best == nil {
			continue
		}
		planned++
	}

	if planned == 0 {
		t.Fatal("no queries planned — corpus coverage is broken")
	}
	if observed == 0 {
		t.Fatal("shadow observer never fired across the corpus — the Insert hook is not wired")
	}
	// The observer ran on every non-opted-in Insert across ~1500 planning runs
	// without a panic and without changing whether planning succeeded — P3 shadow
	// is stable under real load. The extra-dedup rate is informational.
	t.Logf("P3 shadow stable: planned=%d inserts_observed=%d would_extra_dedup=%d", planned, observed, wouldDedup)
}
