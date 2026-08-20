package properties

import (
	"testing"
)

// A FULL SCAN WITH NO TYPE LIST COSTS THE WHOLE STORE, NOT A CONSTANT.
//
// An empty record-type list on FullUnorderedScanExpression means "scan every
// type in the store" (full_unordered_scan.go), so the honest cardinality is the
// sum over every type — which is exactly what the EMPTY record-type name asks a
// StatisticsProvider for.
//
// Answering with the LeafScanCardinality constant instead is not merely
// imprecise once real statistics exist; it INVERTS. A universal scan would cost
// 1e6 while a typed sibling in the same plan costs its real 1000, so the planner
// would rank the scan-everything leaf as the smaller input and drive the join
// from the wrong side — inside the very expression RFC-236's completeness
// argument is built on.
//
// Both directions are pinned. The behaviour must be UNCHANGED for the providers
// that were in use before RFC-236, or this is a silent cost-model change to
// every existing plan; and it must FOLLOW the data for a provider that knows the
// store's size, or the fix did nothing.

func TestFullScanEmptyTypeListAsksTheProvider(t *testing.T) {
	t.Parallel()
	empty := mustFullUnorderedScanExpression(t, nil, propertyTestFlowedType())

	t.Run("collected statistics: the whole store, not the constant", func(t *testing.T) {
		t.Parallel()
		// Two types, 150 and 250 rows. The store is 400 rows, and 400 is the
		// only answer that is on the data's scale.
		stats := NewCollectedStatistics(map[string]float64{"A": 150, "B": 250})
		got := localCostUnclamped(empty, nil, stats)
		if got.Cardinality != 400 {
			t.Errorf("Cardinality = %v, want 400 (the whole store)\n"+
				"  A constant here ranks a scan-everything leaf against a number on nobody's\n"+
				"  scale, which is what makes it invert beside a typed sibling.", got.Cardinality)
		}
		if want := 400 * ScanCPU; got.CPU != want {
			t.Errorf("CPU = %v, want %v (cardinality x ScanCPU)", got.CPU, want)
		}
	})

	t.Run("the inversion it prevents", func(t *testing.T) {
		t.Parallel()
		// The shape that motivated the change: a universal scan over a SMALL
		// store beside a typed scan over a large one. The universal scan must
		// come out cheaper; under the old constant it did not.
		stats := NewCollectedStatistics(map[string]float64{"SMALL": 10})
		universal := localCostUnclamped(empty, nil, stats)
		typed := localCostUnclamped(
			mustFullUnorderedScanExpression(t, []string{"SMALL"}, propertyTestFlowedType()),
			nil, NewCollectedStatistics(map[string]float64{"SMALL": 1000}))
		if !(universal.Cardinality < typed.Cardinality) {
			t.Errorf("universal scan of a 10-row store costs %v, typed scan of a 1000-row "+
				"table costs %v — the universal scan must be cheaper, and was not",
				universal.Cardinality, typed.Cardinality)
		}
	})

	t.Run("DefaultStatistics is byte-identical to the old constant", func(t *testing.T) {
		t.Parallel()
		// The compatibility half. Every plan built before RFC-236 used one of
		// these, so if either moved, this was a silent cost-model change to the
		// whole corpus rather than a fix.
		// nil is deliberately NOT in this table. localCostUnclamped is not
		// nil-safe as a whole — the non-empty branch would panic on it just the
		// same — and every entry point normalises a nil provider to
		// DefaultStatistics before reaching here. A nil case would pin a guard
		// that existed on one arm only, which is why that guard was removed
		// rather than the other arms made to match it.
		for name, stats := range map[string]StatisticsProvider{
			"DefaultStatistics": DefaultStatistics{},
			"MapStatistics":     MapStatistics{PerType: map[string]float64{"A": 150}},
		} {
			got := localCostUnclamped(empty, nil, stats)
			if got.Cardinality != LeafScanCardinality {
				t.Errorf("%s: Cardinality = %v, want LeafScanCardinality (%v) — the pre-RFC-236 "+
					"providers must be unaffected", name, got.Cardinality, LeafScanCardinality)
			}
		}
	})
}
