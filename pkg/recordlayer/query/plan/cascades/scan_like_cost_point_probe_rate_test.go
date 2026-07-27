package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
)

// TestScanLikeCost_PointProbeChargesFetchRate pins the fix for a real
// production defect found while verifying TestFDB_MultiwayJoinOrder_Nway:
// scanLikeCost's fullBindUnique fast path (the concrete join-ordering leaf
// cost used by concretePlanCost/combineConcreteCost) priced a full-PK or
// full-unique-index equality point-probe at ScanCPU — the rate for an
// AMORTIZED multi-row streaming scan — when the executor proves this is
// actually an ISOLATED, unbatched, unpipelined single round trip: exactly
// the same physical shape as a Fetch (properties.FetchCPU's doc comment
// traces both through key_value_cursor.go, flat_map_cursor.go, and
// split_helper.go). Property-checked across a table of (cardinality) values,
// not one example, since the fix must hold regardless of the underlying
// table's size.
func TestScanLikeCost_PointProbeChargesFetchRate(t *testing.T) {
	t.Parallel()

	eq := predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(5))
	eqRange := predicates.EmptyComparisonRange().Merge(&eq).Range
	comps := []*predicates.ComparisonRange{eqRange}

	for _, card := range []float64{1, 20, 1000, 1_000_000} {
		t.Run("", func(t *testing.T) {
			t.Parallel()
			stats := properties.MapStatistics{PerType: map[string]float64{"T": card}}
			got := scanLikeCost(comps, []string{"T"}, stats, true)
			if got.Cardinality != 1 {
				t.Fatalf("cardinality = %v, want 1 (point probe)", got.Cardinality)
			}
			if got.CPU != properties.FetchCPU {
				t.Fatalf("CPU = %v, want properties.FetchCPU (%v) regardless of table size %v — "+
					"an isolated round trip's cost does not depend on how big the table it probes is",
					got.CPU, properties.FetchCPU, card)
			}
		})
	}
}

// TestScanLikeCost_NonPointProbeCPUUnaffected confirms the fix stays scoped:
// a non-unique (bucket) equality bind must still use the amortized ScanCPU
// rate, unchanged.
func TestScanLikeCost_NonPointProbeCPUUnaffected(t *testing.T) {
	t.Parallel()
	stats := properties.MapStatistics{PerType: map[string]float64{"T": 1_000_000}}
	eq := predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(5))
	eqRange := predicates.EmptyComparisonRange().Merge(&eq).Range
	comps := []*predicates.ComparisonRange{eqRange}

	got := scanLikeCost(comps, []string{"T"}, stats, false)
	if got.Cardinality <= 1 {
		t.Fatalf("cardinality = %v, want a bucket (>1) — non-unique bind is not a point probe", got.Cardinality)
	}
	wantCPU := got.Cardinality * properties.ScanCPU * physicalWrapperCostMultiplier
	if got.CPU != wantCPU {
		t.Fatalf("CPU = %v, want %v (unchanged amortized bucket rate)", got.CPU, wantCPU)
	}
}
