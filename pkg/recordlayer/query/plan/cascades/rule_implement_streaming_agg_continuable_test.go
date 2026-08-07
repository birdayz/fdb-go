package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// TestImplementStreamingAgg_AdmitsDistinctInner is the plan-shape pin for the
// CONTINUABLE-WITHOUT-DUPLICATES precondition, and it pins the RULING: Go's two
// DISTINCT plans — the exact pair Java's ContinuableWithoutDuplicatesProperty
// returns false for — are ADMISSIBLE as a streaming-aggregation inner here,
// because #621 made their dedup sets survive a continuation.
//
// This is the mutation-detectable half of the wiring. Flipping either arm of
// ContinuableWithoutDuplicatesVisitor.selfContinuableWithoutDuplicates to false
// (i.e. importing Java's conclusion without its premise) turns this red, and it
// can only turn red if the rule actually consults the property — so it pins the
// admission call sites as well as the verdict.
//
// It does NOT pin the filter against deletion: with an empty false set the
// filter admits everything, so removing it changes no behaviour. That negative
// result is recorded in TestContinuableWithoutDuplicates_FalseSetIsEmpty rather
// than papered over with a test that cannot fail.
func TestImplementStreamingAgg_AdmitsDistinctInner(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		wrap func(plans.RecordQueryPlan) plans.RecordQueryPlan
	}{
		{
			name: "unordered by-row distinct",
			wrap: func(p plans.RecordQueryPlan) plans.RecordQueryPlan {
				return plans.NewRecordQueryDistinctPlan(p)
			},
		},
		{
			name: "unordered primary-key distinct",
			wrap: func(p plans.RecordQueryPlan) plans.RecordQueryPlan {
				return plans.NewRecordQueryUnorderedPrimaryKeyDistinctPlan(p)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// A physical DISTINCT over a physical scan, memoized as the inner's
			// only physical alternative.
			scan := plans.NewRecordQueryScanPlan([]string{"Orders"}, values.UnknownType, false)
			innerRef := expressions.FinalOf(tc.wrap(scan))
			innerQ := expressions.ForEachQuantifier(innerRef)

			gb := expressions.NewGroupByExpression(
				[]values.Value{&values.FieldValue{Field: "customer_id", Typ: values.UnknownType}},
				[]expressions.AggregateSpec{
					{Function: expressions.AggCount, Operand: &values.FieldValue{Field: "id", Typ: values.UnknownType}},
				},
				innerQ,
			)

			results := FireExpressionRule(NewImplementStreamingAggregationRule(), expressions.InitialOf(gb))
			if len(results) == 0 {
				t.Fatalf("GROUP BY over a %s produced NO plan. Go's DISTINCT plans carry their "+
					"dedup set across a continuation through the ExecutionScratch (#621), so they "+
					"are continuable-without-duplicates and must be admitted as a streaming-"+
					"aggregation inner. Declining them imports Java's conclusion without its "+
					"premise — and because streaming aggregation is Go's ONLY aggregation "+
					"strategy, declining does not fall back to a hash aggregation, it means the "+
					"query cannot be planned at all", tc.name)
			}

			// The admission helper is the single point every selection site
			// routes through; pin its verdict directly so a red above can be
			// told apart from an unrelated planning failure.
			if !admissibleStreamingAggInner(tc.wrap(scan)) {
				t.Fatalf("admissibleStreamingAggInner declined a %s", tc.name)
			}
		})
	}
}

// TestImplementStreamingAgg_AdmissionRejectsNonPlans pins the other half of the
// admission helper's contract: a member that is not a physical plan is not an
// admissible inner. Without this, the helper's nil/non-plan arms are unpinned
// and a refactor could turn them into a silent "admit everything".
func TestImplementStreamingAgg_AdmissionRejectsNonPlans(t *testing.T) {
	t.Parallel()

	if admissibleStreamingAggInner(nil) {
		t.Fatal("admissibleStreamingAggInner(nil) = true, want false — a missing member is not " +
			"an admissible streaming-aggregation inner")
	}
	logical := expressions.NewFullUnorderedScanExpression([]string{"Orders"}, values.UnknownType)
	if admissibleStreamingAggInner(logical) {
		t.Fatal("admissibleStreamingAggInner admitted a LOGICAL expression — streaming " +
			"aggregation runs over a physical plan, and admitting a logical member would let " +
			"the rule yield an aggregate over something that cannot execute")
	}
}
