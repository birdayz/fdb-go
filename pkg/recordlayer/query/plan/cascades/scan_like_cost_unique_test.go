package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/properties"
)

// TestScanLikeCost_UniqueGating pins the codex-P2 fix (RFC-069): a fully
// equality-bound access in the concrete join-ordering leaf cost is priced as a
// single row ONLY when provably unique. A primary-key scan / unique index passes
// fullBindUnique=true; a non-unique secondary index passes false and must be
// priced as a BUCKET (table cardinality × selectivity), not a 1-row point probe —
// otherwise join ordering drives off a large non-unique bucket as if it were one
// row. concretePlanCost wires the PK case to true and the index case to
// indexMetadata(pl, ctx).IsUnique (nil ctx → false); this exercises the gate.
func TestScanLikeCost_UniqueGating(t *testing.T) {
	t.Parallel()

	stats := properties.MapStatistics{PerType: map[string]float64{"T": 1_000_000}}
	eq := predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(5))
	eqRange := predicates.EmptyComparisonRange().Merge(&eq).Range
	comps := []*predicates.ComparisonRange{eqRange}

	// Provably unique full-equality bind → exactly 1 row.
	if got := scanLikeCost(comps, []string{"T"}, stats, true).Cardinality; got != 1 {
		t.Fatalf("unique full-equality bind: cardinality=%v, want 1", got)
	}

	// Non-unique full-equality bind → a bucket, well above 1.
	nonUniq := scanLikeCost(comps, []string{"T"}, stats, false).Cardinality
	if nonUniq <= 1 {
		t.Fatalf("non-unique full-equality bind: cardinality=%v, want >1 (bucket)", nonUniq)
	}

	// Sanity: the bucket is the table cardinality scaled by equality selectivity
	// and the physical-wrapper discount — far larger than a point probe.
	want := 1_000_000.0 * properties.FilterSelectivity * physicalWrapperCostMultiplier
	if nonUniq != want {
		t.Fatalf("non-unique bucket cardinality=%v, want %v", nonUniq, want)
	}
}
