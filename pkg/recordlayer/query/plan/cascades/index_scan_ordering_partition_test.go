package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// TestToPlanPartitions_SeparatesFixedBoundIndexScanFromSortedUnboundIndexScan
// is the RecordQueryIndexPlan analog of
// TestToPlanPartitions_SeparatesFixedBoundScanFromSortedUnboundScan
// (pk_scan_ordering_partition_test.go, primary scan). That test covers
// PKScanOrdering's firstNonEq loop end-to-end; RecordQueryIndexPlan.HintOrdering
// (ordering.go) carries the SAME firstNonEq loop, just walking index
// columnNames instead of PK Values, and had no partition-level proof of its
// own — only observable today as a missed sort-elision (the in-memory-sort
// fallback preserves correctness), but an off-by-one there (`firstNonEq = i`
// instead of `i+1`) would still let an equality-bound index scan (A Fixed)
// and an unbound scan over the SAME composite index (A Sorted-ascending)
// co-partition, silently discarding whichever member's real (bound)
// ordering was added second from any downstream consumer that reads
// GetOrdering() as one value for the whole partition (e.g.
// RecordQueryInJoinPlan/RecordQueryInUnionPlan's requested-ordering
// enumeration).
func TestToPlanPartitions_SeparatesFixedBoundIndexScanFromSortedUnboundIndexScan(t *testing.T) {
	t.Parallel()

	unbound := plans.NewRecordQueryIndexPlan("IDX", nil, []string{"T"}, values.UnknownType, false).
		WithIndexMetadata([]string{"A", "B"}, []string{"ID"}, false)
	bound := plans.NewRecordQueryIndexPlan("IDX", nil, []string{"T"}, values.UnknownType, false).
		WithScanComparisons([]*predicates.ComparisonRange{pkGateEq(t, int64(7))}).
		WithIndexMetadata([]string{"A", "B"}, []string{"ID"}, false)

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
		t.Fatalf("ToPlanPartitions() = %d partitions, want 2 (Fixed-bound index scan and "+
			"Sorted-unbound index scan must not co-partition)", len(partitions))
	}

	// Identify each partition by its member's plan and check its ordering
	// individually: the unbound scan keeps every index column plus the
	// trimmed PK suffix, the bound scan drops the equality-bound A column.
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
			if !ordering.IsKnown || len(ordering.Keys) != 3 {
				t.Fatalf("unbound partition ordering = %#v, want [A, B, ID]", ordering)
			}
		case bound:
			sawBound = true
			if !ordering.IsKnown || len(ordering.Keys) != 2 {
				t.Fatalf("bound partition ordering = %#v, want [B, ID] only (A dropped as equality-bound)", ordering)
			}
		default:
			t.Fatalf("unexpected plan in partition: %#v", plan)
		}
	}
	if !sawUnbound || !sawBound {
		t.Fatalf("expected one partition per member, sawUnbound=%v sawBound=%v", sawUnbound, sawBound)
	}
}
