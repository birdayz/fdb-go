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
// imprecise once real statistics exist; it INVERTS. One provider prices a whole
// plan, so the universal scan and any typed sibling are drawn from the same map
// and the store total is >= any member — which means the reachable failure is a
// store LARGER than LeafScanCardinality. There the constant makes a scan of
// EVERY type look cheaper than a scan of ONE of them: not an imprecise estimate
// but an impossible one, inside the very expression RFC-236's completeness
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

	t.Run("the inversion it prevents, in the regime that can actually occur", func(t *testing.T) {
		t.Parallel()
		// ONE provider prices the whole plan — newCostWalk carries a single
		// StatisticsProvider — so the universal scan and its typed sibling are
		// always priced from the same map, where the store total is >= any one
		// member. An earlier version of this arm compared a 10-row store against
		// a 1000-row one using TWO providers, a shape no cost walk can produce;
		// it reddened under mutation only because 1e6 beats any number either
		// side could hold, which is a test passing for the wrong reason.
		//
		// The reachable inversion is a store LARGER than LeafScanCardinality.
		// There the constant makes a scan of EVERYTHING look cheaper than a scan
		// of one table inside it, which is not merely imprecise — it is
		// impossible, and the planner acts on it.
		stats := NewCollectedStatistics(map[string]float64{"BIG": 5e6, "SMALL": 10})
		universal := localCostUnclamped(empty, nil, stats)
		typedBig := localCostUnclamped(
			mustFullUnorderedScanExpression(t, []string{"BIG"}, propertyTestFlowedType()),
			nil, stats)
		if !(universal.Cardinality > typedBig.Cardinality) {
			t.Errorf("universal scan costs %v, typed scan of BIG costs %v — scanning every "+
				"type cannot cost LESS than scanning one of them",
				universal.Cardinality, typedBig.Cardinality)
		}
		// And state what the constant would have done, so the arm names the
		// defect rather than only the corrected behaviour.
		if !(LeafScanCardinality < typedBig.Cardinality) {
			t.Fatalf("this fixture no longer reproduces the inversion: LeafScanCardinality "+
				"(%v) must be BELOW the typed scan's cost (%v) for the constant to have "+
				"made the universal scan look cheaper", LeafScanCardinality, typedBig.Cardinality)
		}
	})

	t.Run("DefaultStatistics is byte-identical to the old constant", func(t *testing.T) {
		t.Parallel()
		// The compatibility half. Every plan built before RFC-236 used one of
		// these, so if either moved, this was a silent cost-model change to the
		// whole corpus rather than a fix.
		// nil is deliberately NOT in this table. localCostUnclamped is not
		// nil-safe as a whole — the non-empty branch would panic on it just the
		// same — so a nil case would have pinned a guard that existed on one arm
		// only, which is why that guard was removed rather than the other arms
		// made to match it.
		//
		// Scope, because the tempting stronger claim is false: the callers that
		// REACH localCostUnclamped normalise nil to DefaultStatistics. Exported
		// CostWithinBounds does not, so "every entry point normalises" would be
		// wrong. Nil-safety across this package is a separate question from what
		// this arm pins.
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
