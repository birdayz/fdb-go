package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// TestToPlanPartitions_SeparatesFixedBoundScanFromSortedUnboundScan is the
// end-to-end regression pin for the PKScanOrdering fix
// (pkg/recordlayer/query/plan/plans/ordering.go): a per-binding equality PK
// scan (id Fixed) and a fully unbound PK scan (id Sorted-ascending) over the
// same PK columns must land in DIFFERENT plan partitions.
//
// Before the fix, PKScanOrdering returned every PK column as a Keys entry
// regardless of any equality comparison, so both scans reported the
// identical Ordering{Keys:[ID,K], Descending:[false,false]}. Since
// DistinctRecords and StoredRecord are identical for both (both are bare
// RecordQueryScanPlans), Ordering was the ONLY property that could have told
// them apart — see toPartitionsFromMap's bucket key in
// expression_partition.go, which folds exactly {distinct, stored,
// orderingHash+orderingsEqual}. With the bug, they co-partitioned into ONE
// PlanPartition; whichever member happened to be added to the
// PlanPropertiesMap first became the partition's representative Ordering,
// silently discarding the other member's real (bound) shape from any
// downstream consumer that reads GetOrdering() as one value for the whole
// partition (e.g. RecordQueryInJoinPlan/RecordQueryInUnionPlan's
// requested-ordering enumeration).
func TestToPlanPartitions_SeparatesFixedBoundScanFromSortedUnboundScan(t *testing.T) {
	t.Parallel()
	id := &values.FieldValue{Field: "ID", Typ: values.NotNullLong}
	k := &values.FieldValue{Field: "K", Typ: values.NotNullLong}

	unbound := plans.NewRecordQueryScanPlan([]string{"TBL"}, values.UnknownType, false).
		WithPrimaryKey([]values.Value{id, k})
	bound := plans.NewRecordQueryScanPlan([]string{"TBL"}, values.UnknownType, false).
		WithPrimaryKey([]values.Value{id, k}).
		WithScanComparisons([]*predicates.ComparisonRange{pkGateEq(t, int64(7))})

	// Sanity: both members must actually agree on DistinctRecords/StoredRecord
	// so the test isolates Ordering as the separating property, not an
	// unrelated dimension.
	unboundProps := computeWrapperProperties(unbound)
	boundProps := computeWrapperProperties(bound)
	if unboundProps.GetBool(properties.PropDistinctRecords) != boundProps.GetBool(properties.PropDistinctRecords) {
		t.Fatalf("DistinctRecords differs between members — test no longer isolates Ordering")
	}
	if unboundProps.GetBool(properties.PropStoredRecord) != boundProps.GetBool(properties.PropStoredRecord) {
		t.Fatalf("StoredRecord differs between members — test no longer isolates Ordering")
	}

	ref := expressions.InitialOf(unbound)
	ref.Insert(bound)
	pm := NewPlanPropertiesMap()
	pm.Add(unbound)
	pm.Add(bound)
	ref.SetPlanProperties(pm)

	partitions := ToPlanPartitions(ref)
	if len(partitions) != 2 {
		t.Fatalf("ToPlanPartitions() = %d partitions, want 2 (Fixed-bound scan and "+
			"Sorted-unbound scan must not co-partition)", len(partitions))
	}

	// Identify each partition by its member's plan and check its ordering
	// individually: the unbound scan keeps both PK columns, the bound scan
	// drops the equality-bound ID column.
	var sawUnbound, sawBound bool
	for _, part := range partitions {
		exprs := part.GetExpressions()
		if len(exprs) != 1 {
			t.Fatalf("partition has %d members, want 1 each (no accidental merge)", len(exprs))
		}
		plan := exprs[0].(physicalPlanExpression).GetRecordQueryPlan()
		ordering := part.GetOrdering()
		switch plan {
		case unbound:
			sawUnbound = true
			if !ordering.IsKnown || len(ordering.Keys) != 2 {
				t.Fatalf("unbound partition ordering = %#v, want [ID, K]", ordering)
			}
		case bound:
			sawBound = true
			if !ordering.IsKnown || len(ordering.Keys) != 1 || ordering.Keys[0] != k {
				t.Fatalf("bound partition ordering = %#v, want [K] only (ID dropped as equality-bound)", ordering)
			}
		default:
			t.Fatalf("unexpected plan in partition: %#v", plan)
		}
	}
	if !sawUnbound || !sawBound {
		t.Fatalf("expected one partition per member, sawUnbound=%v sawBound=%v", sawUnbound, sawBound)
	}
}
