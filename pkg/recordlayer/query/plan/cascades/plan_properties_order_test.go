package cascades

import (
	"fmt"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// TestPlanPropertiesMap_InsertionOrder verifies that Expressions()
// returns wrappers in the exact order they were Add()ed. Without the
// insertion-order tracking (the `order` field), Go map iteration
// would randomise the result — so we repeat the check 10 times to
// catch that class of bug with high probability.
func TestPlanPropertiesMap_InsertionOrder(t *testing.T) {
	t.Parallel()

	const n = 5

	for attempt := 0; attempt < 10; attempt++ {
		pm := NewPlanPropertiesMap()
		wrappers := make([]physicalPlanExpression, n)
		for i := 0; i < n; i++ {
			scan := plans.NewRecordQueryScanPlan(
				[]string{fmt.Sprintf("Type%d", i)},
				values.UnknownType,
				false,
			)
			wrappers[i] = &physicalScanWrapper{plan: scan}
			pm.Add(wrappers[i])
		}

		got := pm.Expressions()
		if len(got) != n {
			t.Fatalf("attempt %d: Expressions() length = %d, want %d", attempt, len(got), n)
		}
		for i, expr := range got {
			if expr != wrappers[i] {
				t.Fatalf("attempt %d: Expressions()[%d] wrong — insertion order not preserved", attempt, i)
			}
		}
	}
}

// TestPlanPropertiesMap_DuplicateAdd verifies that Add()ing the same
// wrapper twice does not create a duplicate in the insertion-order
// slice. The wrapper should appear exactly once, at its original
// (first) position.
func TestPlanPropertiesMap_DuplicateAdd(t *testing.T) {
	t.Parallel()

	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	w := &physicalScanWrapper{plan: scan}

	pm := NewPlanPropertiesMap()
	pm.Add(w)
	pm.Add(w) // duplicate

	got := pm.Expressions()
	if len(got) != 1 {
		t.Fatalf("Expressions() length = %d after duplicate Add, want 1", len(got))
	}
	if got[0] != w {
		t.Fatal("Expressions()[0] should be the original wrapper")
	}
}

// TestToPartitionsFromMap_DeterministicOrder verifies that
// toPartitionsFromMap produces the same partition list (same order,
// same contents) on every call for a given PlanPropertiesMap. Without
// insertion-order preservation, Go map randomisation would cause
// partition ordering and per-partition expression ordering to vary
// across calls, leading to flaky plan selection.
func TestToPartitionsFromMap_DeterministicOrder(t *testing.T) {
	t.Parallel()

	// Build a PlanPropertiesMap with wrappers that fall into
	// different partitions:
	//   - physicalScanWrapper  → distinct=true,  stored=true
	//   - physicalStreamingAggWrapper → distinct=false, stored=false
	// Interleave them so insertion order matters.
	scanA := plans.NewRecordQueryScanPlan([]string{"A"}, values.UnknownType, false)
	scanB := plans.NewRecordQueryScanPlan([]string{"B"}, values.UnknownType, false)
	scanC := plans.NewRecordQueryScanPlan([]string{"C"}, values.UnknownType, false)
	aggD := plans.NewRecordQueryStreamingAggregationPlan(nil, nil, nil)
	aggE := plans.NewRecordQueryStreamingAggregationPlan(nil, nil, nil)

	wA := &physicalScanWrapper{plan: scanA}
	wB := &physicalScanWrapper{plan: scanB}
	wC := &physicalScanWrapper{plan: scanC}
	wD := &physicalStreamingAggWrapper{plan: aggD}
	wE := &physicalStreamingAggWrapper{plan: aggE}

	pm := NewPlanPropertiesMap()
	// Interleave scan and agg wrappers.
	pm.Add(wA)
	pm.Add(wD)
	pm.Add(wB)
	pm.Add(wE)
	pm.Add(wC)

	// Capture the reference result.
	refPartitions := toPartitionsFromMap(pm)
	if len(refPartitions) == 0 {
		t.Fatal("toPartitionsFromMap returned 0 partitions")
	}

	// Snapshot: for each partition, record its expression pointers.
	type snapshot struct {
		exprs []expressions.RelationalExpression
	}
	refSnap := make([]snapshot, len(refPartitions))
	for i, p := range refPartitions {
		refSnap[i] = snapshot{exprs: p.GetExpressions()}
	}

	// Repeat 20 times and verify identical output.
	for attempt := 0; attempt < 20; attempt++ {
		partitions := toPartitionsFromMap(pm)
		if len(partitions) != len(refPartitions) {
			t.Fatalf("attempt %d: partition count = %d, want %d",
				attempt, len(partitions), len(refPartitions))
		}
		for i, p := range partitions {
			exprs := p.GetExpressions()
			if len(exprs) != len(refSnap[i].exprs) {
				t.Fatalf("attempt %d: partition[%d] expression count = %d, want %d",
					attempt, i, len(exprs), len(refSnap[i].exprs))
			}
			for j, e := range exprs {
				if e != refSnap[i].exprs[j] {
					t.Fatalf("attempt %d: partition[%d].exprs[%d] differs — order not deterministic",
						attempt, i, j)
				}
			}
		}
	}
}
