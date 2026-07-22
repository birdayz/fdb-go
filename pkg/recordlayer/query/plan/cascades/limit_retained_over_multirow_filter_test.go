package cascades

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestPlanner_LimitOneOverMultiRowFilterRetainsLimit pins RFC-188 finding 2:
// a LIMIT 1 over a filter that can return MANY rows must keep its Limit — the
// planner must never strip it. The deleted RemoveRangeOneRule dropped the Limit
// whenever the inner's HEURISTIC cardinality estimate underflowed below 1.0
// (LeafScanCardinality 1e6 * FilterSelectivity 0.5^numPreds; at 24 separate
// predicates that is ~0.06 < 1.0), asserting a FALSE equivalence between
// `Limit(1, filter)` and `filter`. When the rule fired the full planner
// produced `PredicatesFilter(Scan, [24 preds])` with no Limit → all matching
// rows instead of one. A correctness transform gated on a made-up selectivity
// constant. The SQL surface masks this today (conjunctions collapse to one
// AndPredicate, numPreds=1, estimate 5e5, rule never fires), but the memo-level
// firing is real and any conjunct-splitting normalization would detonate it.
//
// RED before the rule's deletion (plan lacks "Limit"); GREEN after (retained).
func TestPlanner_LimitOneOverMultiRowFilterRetainsLimit(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	// 24 SEPARATE predicates keep numPreds high enough that the (deleted)
	// heuristic estimate 1e6*0.5^24 ≈ 0.06 underflows below 1.0.
	preds := make([]predicates.QueryPredicate, 24)
	for i := range preds {
		preds[i] = predicates.NewComparisonPredicate(
			&values.FieldValue{Field: "A", Typ: values.UnknownType},
			predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(i)),
		)
	}
	filter := expressions.NewLogicalFilterExpression(preds, scanQ)
	filterRef := expressions.InitialOf(filter)
	filterQ := expressions.ForEachQuantifier(filterRef)
	lim := expressions.NewLogicalLimitExpression(1, 0, filterQ)

	plan := planPipeline(t, lim)
	if !strings.Contains(plan, "Limit") {
		t.Fatalf("LIMIT 1 over a multi-row filter was stripped from the plan "+
			"(a semantic limit deleted on a heuristic estimate): %s", plan)
	}
}
