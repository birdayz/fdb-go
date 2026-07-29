package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
)

// TestAggregateCandidateDeclinedByRawIndexShortcuts pins the admission gates
// that keep an aggregate index out of the planner's raw, name-based index
// shortcuts.
//
// WHY THIS EXISTS. An aggregate index entry has NO backing record: its key is
// only the grouping key and its value is the running aggregate, so
// Index.getEntryPrimaryKey yields an EMPTY tuple. Three rules take a shortcut
// that turns a MatchCandidate straight into a bare, record-FETCHING
// RecordQueryIndexPlan, bypassing the traversal-based data-access path and the
// index-type gating in the candidate enumerator:
//
//	rule_implement_nested_loop_join.go  correlated EXISTS probe
//	rule_ordered_index_scan.go          ORDER BY served from index order
//	rule_streaming_agg_from_index.go    GROUP BY served from index order
//
// If an AggregateIndexMatchCandidate is admitted to any of them, the resulting
// plan scans the aggregate index and then fetches a record per entry — from an
// empty primary key — and execution aborts with Java's
// IndexOrphanBehavior.ERROR fault:
//
//	record not found from index entry (index_name=..., primary_key=(), index_key=(0))
//
// That is exactly what shipped: the correlated-EXISTS shortcut selected a SUM
// index whose grouping key was the join column, because it matched candidates
// on first-column NAME alone and never looked at the candidate's TYPE.
//
// Java cannot express this defect at all — an aggregate index type is opt-IN to
// candidacy there. IndexMaintainerFactory.createMatchCandidates defaults to
// Collections.emptyList (IndexMaintainerFactory.java:117-120), and
// AtomicMutationIndexMaintainerFactory (which owns SUM/COUNT/MIN_EVER/...)
// overrides it to return ONLY expandAggregateIndexMatchCandidate
// (AtomicMutationIndexMaintainerFactory.java:163-169), so
// expandValueIndexMatchCandidate is unreachable for a SUM index. Go's
// enumeration is opt-OUT, which is why these per-site gates carry the weight.
//
// Each of the three gates below declines for a DIFFERENT reason, and only one
// of them names aggregates. They are asserted separately so that relaxing any
// one of them fails here rather than in a nightly stress run.
func TestAggregateCandidateDeclinedByRawIndexShortcuts(t *testing.T) {
	t.Parallel()

	// The shape that broke: SUM(amount) GROUP BY customer_id, probed by a
	// correlated equality on the grouping key (o.customer_id = c.id).
	agg := NewAggregateIndexMatchCandidate(
		"SUM_AMOUNT_BY_CUSTOMER",
		[]string{"ORDERS"},
		[]string{"CUSTOMER_ID"},
		expressions.AggSum,
		"AMOUNT",
	)

	// Precondition: the candidate really does advertise the join column as its
	// leading column. Without this the test could pass vacuously — the
	// shortcuts skip anything whose first column does not match the predicate,
	// so a candidate with no columns would be declined for the wrong reason.
	cols := agg.GetColumnNames()
	if len(cols) != 1 || cols[0] != "CUSTOMER_ID" {
		t.Fatalf("GetColumnNames() = %v, want [CUSTOMER_ID] — the gates below "+
			"would decline for the wrong reason and assert nothing", cols)
	}

	t.Run("plain_field_shortcut_gate", func(t *testing.T) {
		t.Parallel()
		// Gate used by the correlated-EXISTS probe (the shape that shipped the
		// bug) — it admits only *ValueIndexScanMatchCandidate.
		got, ok := candidatePlainFieldColumnsForShortcut(agg)
		if ok {
			t.Errorf("candidatePlainFieldColumnsForShortcut admitted the aggregate "+
				"candidate (cols=%v) — the correlated-EXISTS shortcut will build a "+
				"record-fetching IndexScan over an index whose entries have no "+
				"backing record", got)
		}
	})

	t.Run("base_record_cardinality_gate", func(t *testing.T) {
		t.Parallel()
		// Gate used by the ORDER BY shortcut, which does NOT apply the
		// plain-field gate. An aggregate candidate is declined only because it
		// implements neither DistinctRecordsSignal nor CreatesDuplicates, so
		// the tri-state falls through to "unknown". Adding either method to
		// AggregateIndexMatchCandidate re-arms the ORDER BY shortcut; this
		// assertion is what catches that.
		if candidatePreservesBaseRecordCardinality(agg) {
			t.Error("candidatePreservesBaseRecordCardinality admitted the aggregate " +
				"candidate — rule_ordered_index_scan will serve ORDER BY from a raw " +
				"scan of the aggregate index and fetch records from empty primary keys")
		}
	})

	t.Run("streaming_agg_gate_is_explicit", func(t *testing.T) {
		t.Parallel()
		// The GROUP BY shortcut names the type outright. Assert the type
		// identity the rule switches on, so a refactor that renames or
		// re-parents the candidate cannot silently drop that arm.
		var mc MatchCandidate = agg
		if _, isAgg := mc.(*AggregateIndexMatchCandidate); !isAgg {
			t.Error("an AggregateIndexMatchCandidate no longer type-asserts as one — " +
				"rule_streaming_agg_from_index's explicit isAgg guard is now dead")
		}
	})
}

// TestAggregateCandidateScanPlanNeedsAggregateWrapper pins WHY the gates above
// matter: AggregateIndexMatchCandidate.ToScanPlan returns a bare
// RecordQueryIndexPlan — the same record-fetching plan a value index produces.
// It is safe only because its one legitimate caller,
// AggregateDataAccessRule, immediately wraps it in a
// RecordQueryAggregateIndexPlan, which reads the aggregate out of the entry
// VALUE and never fetches a record.
//
// Any new caller that uses the returned plan directly reintroduces the fault.
// If ToScanPlan is ever changed to return an already-aggregate-aware plan, this
// test should be updated to assert that instead — it is deliberately pinning
// the sharp edge, not endorsing it.
func TestAggregateCandidateToScanPlanIsARawFetchingIndexPlan(t *testing.T) {
	t.Parallel()

	agg := NewAggregateIndexMatchCandidate(
		"SUM_AMOUNT_BY_CUSTOMER",
		[]string{"ORDERS"},
		[]string{"CUSTOMER_ID"},
		expressions.AggSum,
		"AMOUNT",
	)

	plan := agg.ToScanPlan(nil, false)
	if plan == nil {
		t.Fatal("ToScanPlan returned nil")
	}
	idx := extractIndexPlan(plan)
	if idx == nil {
		t.Fatalf("ToScanPlan produced %T, expected a plan wrapping a "+
			"RecordQueryIndexPlan", plan)
	}
	if got := idx.GetIndexName(); got != "SUM_AMOUNT_BY_CUSTOMER" {
		t.Errorf("index name = %q, want SUM_AMOUNT_BY_CUSTOMER", got)
	}
}
