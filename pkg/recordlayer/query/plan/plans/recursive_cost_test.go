package plans

import (
	"math"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestRecursiveCost_DfsStrictlyCheaperThanLevelUnion pins the RFC-184 W2
// safety property: for IDENTICAL children the DFS join must cost strictly less
// than the level-at-a-time union, so the planner prefers DFS on COST alone —
// no wrapper Go type, no compareRecursiveCTE structural rung required. The
// preference lives in the cost model (plans.RecordQueryRecursiveLevelUnionPlan's
// level-buffer echo term), so it survives deletion of the Cascades physical
// wrappers.
func TestRecursiveCost_DfsStrictlyCheaperThanLevelUnion(t *testing.T) {
	t.Parallel()

	dfs := NewRecordQueryRecursiveDfsJoinPlan(nil, nil,
		values.NamedCorrelationIdentifier("prior"), DfsPreorder)
	level := NewRecordQueryRecursiveLevelUnionPlan(nil, nil,
		values.NamedCorrelationIdentifier("scan"),
		values.NamedCorrelationIdentifier("insert"))

	// A spread of seed/recursive child-cost shapes: tiny, wide-CPU, and the
	// zero-cardinality clamp path (both children 0 → LeafScanCardinality).
	cases := []struct {
		name  string
		child []properties.Cost
	}{
		{"small", []properties.Cost{{Cardinality: 10, CPU: 1}, {Cardinality: 5, CPU: 2}}},
		{"wide-cpu", []properties.Cost{{Cardinality: 1000, CPU: 500}, {Cardinality: 200, CPU: 300}}},
		{"zero-clamp", []properties.Cost{{Cardinality: 0, CPU: 0}, {Cardinality: 0, CPU: 0}}},
		{"asymmetric", []properties.Cost{{Cardinality: 3, CPU: 0.5}, {Cardinality: 400, CPU: 12}}},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dfsCost := dfs.HintCost(tc.child, properties.DefaultStatistics{})
			levelCost := level.HintCost(tc.child, properties.DefaultStatistics{})

			// (1) The whole point: DFS is strictly cheaper.
			if !dfsCost.Less(levelCost) {
				t.Fatalf("DFS must cost strictly LESS than LevelUnion for identical children\n  dfs   = %+v (total %g)\n  level = %+v (total %g)",
					dfsCost, dfsCost.Total(), levelCost, levelCost.Total())
			}

			// (2) OUTPUT cardinality must be IDENTICAL — the union emits the
			// same rows as the DFS join; the echo is CPU-only, so parent
			// roll-up is unaffected.
			if dfsCost.Cardinality != levelCost.Cardinality {
				t.Fatalf("output cardinality must match (echo is CPU-only): dfs %g, level %g",
					dfsCost.Cardinality, levelCost.Cardinality)
			}

			// (3) The gap must equal the level-buffer echo term — pins the
			// sizing (per-row buffer write+read-back at the union merge rate),
			// not just "some positive nudge". Tolerance-based: gotEcho is a
			// DIFFERENCE of two independently-rounded sums (levelCost.CPU and
			// dfsCost.CPU each accumulate their own float64 rounding), so it
			// need not bit-exactly equal a fresh product even when the true
			// values are mathematically identical.
			wantEcho := dfsCost.Cardinality * properties.UnionCPU * levelUnionBufferTouches
			gotEcho := levelCost.CPU - dfsCost.CPU
			if math.Abs(gotEcho-wantEcho) > 1e-9 {
				t.Fatalf("echo term mismatch: got %g, want %g (card %g × UnionCPU %g × touches %d)",
					gotEcho, wantEcho, dfsCost.Cardinality, properties.UnionCPU, levelUnionBufferTouches)
			}
			if wantEcho <= 0 {
				t.Fatalf("echo term must be strictly positive, got %g", wantEcho)
			}
		})
	}
}

// TestRecursiveCost_DfsMatchesBaseRecursion guards that the DFS cost is the bare
// recursiveCost (no accidental echo term on the streaming operator).
func TestRecursiveCost_DfsMatchesBaseRecursion(t *testing.T) {
	t.Parallel()
	dfs := NewRecordQueryRecursiveDfsJoinPlan(nil, nil,
		values.NamedCorrelationIdentifier("prior"), DfsPreorder)
	child := []properties.Cost{{Cardinality: 42, CPU: 7}, {Cardinality: 9, CPU: 3}}

	got := dfs.HintCost(child, properties.DefaultStatistics{})
	want := recursiveCost(child)
	if got != want {
		t.Fatalf("DFS HintCost must equal bare recursiveCost: got %+v, want %+v", got, want)
	}
}
