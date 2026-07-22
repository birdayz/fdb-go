package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// TestComputeCardinalities_PrimaryPointLookupBounded pins RFC-189 B1
// (finding 10 M3-followup): computeCardinalities' RecordQueryScanPlan arm always
// returned UnknownMaxCardinality, so a full-PK-equality primary scan (a point
// lookup, max 1 — which Java's CardinalitiesProperty bounds) was reported
// unknown, and the M3 whole-plan-cardinality outer guard over-abstained for a
// bounded-primary-vs-unbounded comparison. The scan arm now bounds a full-PK
// equality bind at 1, cross-checking len(comps) == len(PK) so a prefix bind
// (which can match many rows) stays unknown — the safe direction.
func TestComputeCardinalities_PrimaryPointLookupBounded(t *testing.T) {
	t.Parallel()

	// Two PK columns → a composite primary key. The values' contents are
	// irrelevant to the length-coverage check.
	pk2 := []values.Value{
		&values.ConstantValue{Value: int64(0), Typ: values.NullableLong},
		&values.ConstantValue{Value: int64(0), Typ: values.NullableLong},
	}

	t.Run("full_pk_equality_is_point_lookup", func(t *testing.T) {
		t.Parallel()
		scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false).
			WithScanComparisons([]*predicates.ComparisonRange{pkGateEq(t, int64(7)), pkGateEq(t, int64(9))}).
			WithPrimaryKey(pk2)
		if !wholePlanMaxCardinalityKnown(scan) {
			t.Fatal("full-PK-equality primary scan must be a bounded point lookup (max 1)")
		}
	})

	t.Run("prefix_bind_stays_unknown", func(t *testing.T) {
		t.Parallel()
		// One equality comparison against a 2-column PK → a prefix scan that can
		// match many rows. scanProvableMaxCard alone would over-bound it
		// (numBound == len(comps)); the len(comps) == len(PK) cross-check rejects.
		scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false).
			WithScanComparisons([]*predicates.ComparisonRange{pkGateEq(t, int64(7))}).
			WithPrimaryKey(pk2)
		if wholePlanMaxCardinalityKnown(scan) {
			t.Fatal("a composite-PK prefix bind must stay unknown (matches many rows)")
		}
	})

	t.Run("no_pk_values_stays_unknown", func(t *testing.T) {
		t.Parallel()
		// All-equality but PK metadata absent → full coverage unprovable → unknown.
		scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false).
			WithScanComparisons([]*predicates.ComparisonRange{pkGateEq(t, int64(7))})
		if wholePlanMaxCardinalityKnown(scan) {
			t.Fatal("without PK values, full-PK coverage is unprovable → unknown")
		}
	})

	t.Run("bare_full_scan_is_unknown", func(t *testing.T) {
		t.Parallel()
		scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
		if wholePlanMaxCardinalityKnown(scan) {
			t.Fatal("bare full scan must be unknown")
		}
	})
}
