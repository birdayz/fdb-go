package cascades

import (
	"math"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// TestPrimaryScanMatchCandidate_RecordTypeKeyPrefixEliminatesTypeFilter pins
// the fix for the join-order regression diagnosed against
// TestFDB_MultiwayJoinOrder_Nway: a primary-scan candidate whose key is
// ordered by the record-type key (the default for every SQL-created table
// that does not opt into intermingled storage — see
// buildPrimaryKeyExpression in pkg/relational/core/metadata/builder.go)
// physically cannot return rows of any type but the one queried, because
// executeScan prefixes the FDB key range with the record-type ordinal. A
// TypeFilterPlan wrapped on top of such a scan is pure overhead — worse, it
// was earning TypeFilterCost's 0.5 selectivity discount on a filter that
// discards nothing, and three such redundant hops compounded into 0.125,
// making a genuinely worse backward join order look cheaper than the
// correct forward order.
//
// Mirrors Java's PrimaryScanMatchCandidate.toEquivalentPlan()
// (fdb-record-layer-core .../cascades/PrimaryScanMatchCandidate.java):
//
//	if (hasAndOrderedByRecordTypeKey()) {
//	    return scanPlan;
//	}
//	return new RecordQueryTypeFilterPlan(...);
//
// Java ELIMINATES the redundant filter at plan-construction time rather
// than costing it with a discount it has not earned — this test pins the
// same behavior on the Go port: elimination, not a costed discount.
func TestPrimaryScanMatchCandidate_RecordTypeKeyPrefixEliminatesTypeFilter(t *testing.T) {
	t.Parallel()

	tAlias := values.NamedCorrelationIdentifier("t")
	eq := pkGateEq(t, int64(7))
	prefix := map[values.CorrelationIdentifier]*predicates.ComparisonRange{tAlias: eq}

	t.Run("redundant (record-type-key-prefixed PK): no TypeFilterPlan, no discount", func(t *testing.T) {
		t.Parallel()
		candidate := NewPrimaryScanMatchCandidate(
			nil,
			[]values.CorrelationIdentifier{tAlias},
			[]string{"T", "U"}, // available: two types share the store
			[]string{"T"},      // queried: one of them
			[]string{"ID"},
			true, // hasRecordTypeKeyPrefix — the scan is already type-scoped
			values.UnknownType,
		)
		plan := candidate.ToScanPlan(prefix, false)

		if _, wrapped := plan.(*plans.RecordQueryTypeFilterPlan); wrapped {
			t.Fatalf("plan = %T, want a bare scan — a record-type-key-prefixed PK scan "+
				"cannot yield any type but the one queried, so wrapping it in a TypeFilterPlan "+
				"is redundant and must not happen", plan)
		}
		scan, ok := plan.(*plans.RecordQueryScanPlan)
		if !ok {
			t.Fatalf("plan = %T, want *plans.RecordQueryScanPlan", plan)
		}

		// A single-column PK, fully equality-bound, prices as a point probe
		// (cardinality 1) regardless of the store's statistics — same as the
		// "fully bound" leg of TestScanPlanExpressionHintCost_StampedPartialPKPrefixNotPointProbe.
		// The point of this assertion is the ABSENCE of TypeFilterSelectivity
		// on top of it.
		stats := properties.FixedStatistics{Cardinality: 1000}
		got := (&scanPlanExpression{plan: scan}).HintCost(nil, stats)
		if got.Cardinality != 1 {
			t.Fatalf("redundant-filter-eliminated cardinality = %v, want 1 (no TypeFilterSelectivity factor on top)",
				got.Cardinality)
		}
	})

	t.Run("discriminating (no record-type-key prefix): TypeFilterPlan wraps, discount applies", func(t *testing.T) {
		t.Parallel()
		candidate := NewPrimaryScanMatchCandidate(
			nil,
			[]values.CorrelationIdentifier{tAlias},
			[]string{"T", "U"}, // available: two types genuinely share this PK range
			[]string{"T"},      // queried: one of them
			[]string{"ID"},
			false, // no record-type-key prefix — a real shared-PK-range multi-type scan
			values.UnknownType,
		)
		plan := candidate.ToScanPlan(prefix, false)

		tf, wrapped := plan.(*plans.RecordQueryTypeFilterPlan)
		if !wrapped {
			t.Fatalf("plan = %T, want a TypeFilterPlan — the scan can genuinely yield "+
				"other available types and the filter is load-bearing", plan)
		}

		// Same fully-bound point probe underneath (base cardinality 1), but
		// now the TypeFilterSelectivity discount applies ON TOP because the
		// filter is load-bearing.
		stats := properties.FixedStatistics{Cardinality: 1000}
		got := (&scanPlanExpression{plan: tf}).HintCost(nil, stats)
		want := properties.TypeFilterSelectivity
		if math.Abs(got.Cardinality-want) > 1e-12 {
			t.Fatalf("discriminating-filter cardinality = %v, want %v (TypeFilterSelectivity earned)",
				got.Cardinality, want)
		}
	})
}
