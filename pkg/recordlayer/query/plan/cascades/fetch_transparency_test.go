package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// TestComputeDistinctRecords_FetchIsTransparent pins Fetch transparency: a
// Fetch (RecordQueryFetchFromPartialRecordPlan) is 1:1 — one base record per
// index entry — so Java treats it as transparent in DistinctRecordsProperty (and
// PrimaryKeyProperty / CardinalitiesProperty). Without the transparent arm the
// M4 distinct fact is hidden above the common Fetch(IndexScan), and — worse —
// a fan-out index's distinct=false would be lost, re-opening the over-report.
func TestComputeDistinctRecords_FetchIsTransparent(t *testing.T) {
	t.Parallel()

	distinctThroughFetch := func(createsDuplicates bool) bool {
		idx := plans.NewRecordQueryIndexPlan("idx", nil, []string{"T"}, values.UnknownType, false).
			WithDistinctRecordsSignal(createsDuplicates)
		idxRef := expressions.InitialOf(idx)
		computeRefPlanProperties(idxRef)

		fetch := plans.NewRecordQueryFetchFromPartialRecordPlanFromQuantifier(
			expressions.ForEachQuantifier(idxRef), nil, values.UnknownType, plans.FetchIndexRecordsPrimaryKey)
		fetchRef := expressions.InitialOf(fetch)
		computeRefPlanProperties(fetchRef)

		return GetRefPlanPropertiesMap(fetchRef).GetProperties(fetch).GetBool(properties.PropDistinctRecords)
	}

	// Non-fan-out index (distinct=true) → the Fetch must expose distinct=true.
	if !distinctThroughFetch(false) {
		t.Fatal("Fetch(non-fan-out IndexScan) must pass through distinct=true (Fetch is transparent)")
	}
	// Fan-out index (distinct=false) → the Fetch must NOT report distinct (the
	// over-report must not sneak back in above the Fetch).
	if distinctThroughFetch(true) {
		t.Fatal("Fetch(fan-out IndexScan) must NOT report distinct — transparency preserves false")
	}
}
