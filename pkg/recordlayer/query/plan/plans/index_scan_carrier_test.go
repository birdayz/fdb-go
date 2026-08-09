package plans

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// carrierTestIndexPlan is a minimal index scan to wrap.
func carrierTestIndexPlan(name string) *RecordQueryIndexPlan {
	return NewRecordQueryIndexPlan(name, nil, []string{"T"}, values.UnknownType, false).
		WithIndexMetadata([]string{"A"}, []string{"ID"}, false)
}

// TestIndexPlanOf_SeesThroughTheCoveringWrapper pins the behaviour the whole
// IndexScanCarrier type exists for: a caller asking "what index scan does this
// node read entries from" gets the SAME answer for a bare scan and for the
// covering plan that wraps it.
//
// It is the behavioural half of the docscheck gate. The gate proves no site
// type-tests the bare plan without a covering arm; this proves the accessor
// those sites are pointed at actually answers for both — a gate that redirected
// every site to a helper returning nil for covering would pass while making
// things worse.
func TestIndexPlanOf_SeesThroughTheCoveringWrapper(t *testing.T) {
	t.Parallel()
	idx := carrierTestIndexPlan("IDX_A")
	cov := NewRecordQueryCoveringIndexPlan(idx)

	bare, ok := IndexPlanOf(idx)
	if !ok || bare != idx {
		t.Fatalf("IndexPlanOf(bare index scan) = (%p, %v), want (%p, true)", bare, ok, idx)
	}
	through, ok := IndexPlanOf(cov)
	if !ok {
		t.Fatalf("IndexPlanOf(covering scan) reported NOT an index scan; the covering plan holds "+
			"its scan as a FIELD, so a caller that gets false here concludes there is no index "+
			"scan at all — which downstream reads as an unrestricted scan. plan: %s", cov.Explain())
	}
	if through != idx {
		t.Errorf("IndexPlanOf(covering scan) = %p, want the wrapped scan %p", through, idx)
	}
}

// TestIndexPlanOf_RejectsNonIndexScans pins the other direction. The accessor
// must not answer for a node that is not an index scan, or a caller would read
// facts about an unrelated plan's range.
func TestIndexPlanOf_RejectsNonIndexScans(t *testing.T) {
	t.Parallel()

	// A scan plan is a leaf, but a PRIMARY-KEY one: it has no index.
	scan := NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	if got, ok := IndexPlanOf(scan); ok {
		t.Errorf("IndexPlanOf(primary-key scan) = (%v, true); a table scan reads no index", got)
	}

	// The aggregate index plan ALSO carries a GetIndexPlan() *RecordQueryIndexPlan
	// and would satisfy an unsealed IndexScanCarrier structurally — while
	// emitting one row per GROUP rather than one per entry. The seal is what
	// keeps it out; without it, a caller asking for the scan behind an index
	// access would silently be handed an aggregate scan's inner.
	agg := NewRecordQueryAggregateIndexPlan(carrierTestIndexPlan("IDX_G"), "T", values.UnknownType, "COUNT")
	if got, ok := IndexPlanOf(agg); ok {
		t.Errorf("IndexPlanOf(aggregate index plan) = (%v, true); it emits one row per GROUP, "+
			"not one per entry, so it is not interchangeable with an index scan. The unexported "+
			"seal on IndexScanCarrier is what must keep it out", got)
	}

	// A nil plan is not an index scan and must not panic.
	if got, ok := IndexPlanOf(nil); ok {
		t.Errorf("IndexPlanOf(nil) = (%v, true)", got)
	}
}

// TestIndexPlanOf_TypedNilCarrierIsNotAnIndexScan pins the degenerate shapes.
//
// A typed nil in a non-nil interface is the shape the hint-contract parity
// harnesses enumerate plan types with, and a struct-literal covering plan (a
// test fixture bypassing the constructor) has a nil inner. Both must report
// "not an index scan" rather than handing back a nil pointer with ok=true,
// which a caller dereferences far from the cause.
func TestIndexPlanOf_TypedNilCarrierIsNotAnIndexScan(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		plan RecordQueryPlan
	}{
		{"typed-nil bare index scan", (*RecordQueryIndexPlan)(nil)},
		{"typed-nil covering scan", (*RecordQueryCoveringIndexPlan)(nil)},
		{"covering scan with a nil inner", &RecordQueryCoveringIndexPlan{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := IndexPlanOf(tc.plan)
			if ok {
				t.Errorf("IndexPlanOf(%s) = (%v, true); a caller reads ok=true as licence to "+
					"dereference", tc.name, got)
			}
			if got != nil {
				t.Errorf("IndexPlanOf(%s) returned a non-nil scan %p alongside ok=false", tc.name, got)
			}
		})
	}
}

// TestIndexScanCarrier_BothPlanTypesImplementIt is the compile-time assertion
// restated as a runtime one, so the failure names WHICH type stopped
// implementing rather than only failing to build.
func TestIndexScanCarrier_BothPlanTypesImplementIt(t *testing.T) {
	t.Parallel()
	idx := carrierTestIndexPlan("IDX_A")
	for name, plan := range map[string]RecordQueryPlan{
		"bare index scan": idx,
		"covering scan":   NewRecordQueryCoveringIndexPlan(idx),
	} {
		if _, ok := plan.(IndexScanCarrier); !ok {
			t.Errorf("%s does not implement IndexScanCarrier; every site the docscheck gate "+
				"redirects to plans.IndexPlanOf would silently stop seeing it", name)
		}
	}
}
