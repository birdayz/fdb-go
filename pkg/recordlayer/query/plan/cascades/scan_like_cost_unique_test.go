package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// TestScanLikeCost_UniqueGating pins the RFC-069 fix: a fully
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
	physicalTypes := []values.Type{values.NullableLong}
	if got := scanLikeCost(
		comps, physicalTypes, []string{"T"}, stats, 1, properties.TupleKeyUnique,
	).Cardinality; got != 1 {
		t.Fatalf("unique full-equality bind: cardinality=%v, want 1", got)
	}

	// Non-unique full-equality bind → a bucket, well above 1.
	nonUniq := scanLikeCost(
		comps, physicalTypes, []string{"T"}, stats, 0, properties.TupleKeyUnique,
	).Cardinality
	if nonUniq <= 1 {
		t.Fatalf("non-unique full-equality bind: cardinality=%v, want >1 (bucket)", nonUniq)
	}

	// Sanity: the bucket is the table cardinality scaled by the EQUALITY-bound
	// selectivity — far larger than a point probe. NO physical-wrapper discount:
	// Cardinality is a property of the logical group, not of how many physical
	// nodes wrap the plan, so physicalWrapperCostMultiplier applies to CPU only.
	// (Uses EqualityBoundSelectivity, not FilterSelectivity: an indexed equality
	// bound is a point lookup, not a generic residual filter — RFC-164 COST-SELECTIVITY.)
	want := 1_000_000.0 * properties.EqualityBoundSelectivity
	if nonUniq != want {
		t.Fatalf("non-unique bucket cardinality=%v, want %v", nonUniq, want)
	}
}

func TestConcreteJoinCostUniqueCompositePrefixIsNotPointProbe(t *testing.T) {
	t.Parallel()

	stats := properties.MapStatistics{PerType: map[string]float64{"T": 1_000_000}}
	ctx := NewPlanContextFromIndexDefs([]IndexDef{testIndexDef{
		name: "UNIQUE_AB", columns: []string{"A", "B"},
		recordTypes: []string{"T"}, unique: true,
	}})
	prefix := plans.NewRecordQueryIndexPlan(
		"UNIQUE_AB",
		[]*predicates.ComparisonRange{pkGateEq(t, int64(7))},
		[]string{"T"}, values.UnknownType, false,
	).WithKeyComponentTypes([]values.Type{values.NotNullLong})
	if cost := concretePlanCost(prefix, stats, ctx); cost.Cardinality == 1 {
		t.Fatalf("partial UNIQUE(A,B) prefix priced as point probe: %+v", cost)
	}

	full := plans.NewRecordQueryIndexPlan(
		"UNIQUE_AB",
		[]*predicates.ComparisonRange{
			pkGateEq(t, int64(7)), pkGateEq(t, int64(9)),
		},
		[]string{"T"}, values.UnknownType, false,
	).WithKeyComponentTypes([]values.Type{values.NotNullLong, values.NotNullLong})
	if cost := concretePlanCost(full, stats, ctx); cost.Cardinality != 1 {
		t.Fatalf("fully bound UNIQUE(A,B) not priced as point probe: %+v", cost)
	}
}
