package cascades

// Parity between a plan's ChildrenAsSet/CanCorrelate and its physical
// wrapper's (RFC-183 P5).
//
// These two booleans are the memo's structural contract for an expression:
// ChildrenAsSet decides whether two expressions comparing their children
// pairwise or as a SET are equivalent (so it drives dedup and memo collapse),
// and CanCorrelate decides whether an expression may bind aliases its children
// reference (so it drives correlation propagation). Getting either wrong on one
// side of the pair is not a cosmetic drift — it silently changes what the memo
// considers the same expression, or which bound aliases reach a child.
//
// Seven plan types override the PlanExprBase default of false/false. Nothing
// pinned them: they were transcribed from the wrappers by hand, and a future
// edit to either side could flip one without a single test noticing, because
// the memo asks the WRAPPER and the plan-side value is unreachable until the
// gated wrapper deletion (RFC-183 §11) flips the caller over. This test is that
// missing dimension — it asserts, for every pair, that the two agree.
//
// Both methods are compile-time constants that never touch their receiver, so
// zero-value instances answer them exactly as production ones do; using them
// keeps the table readable and independent of every constructor's signature.
// Should either method ever become state-dependent, this test's zero-value
// receiver is the right place for that change to be noticed.

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func TestPlanWrapperFlagParity(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		plan expressions.RelationalExpression
		wrap expressions.RelationalExpression
		// The expected values, spelled out so the test pins the ACTUAL
		// contract rather than merely pinning the two sides to each other —
		// a simultaneous flip of both would otherwise pass.
		childrenAsSet bool
		canCorrelate  bool
	}{
		// --- the four ChildrenAsSet overrides ---
		{
			name:          "Union",
			plan:          &plans.RecordQueryUnionPlan{},
			wrap:          &physicalUnionWrapper{},
			childrenAsSet: true,
		},
		{
			name:          "UnorderedUnion",
			plan:          &plans.RecordQueryUnorderedUnionPlan{},
			wrap:          &physicalUnorderedUnionWrapper{},
			childrenAsSet: true,
		},
		{
			name:          "Intersection",
			plan:          &plans.RecordQueryIntersectionPlan{},
			wrap:          &physicalIntersectionWrapper{},
			childrenAsSet: true,
		},
		{
			name:          "MergeSortUnion",
			plan:          &plans.RecordQueryMergeSortUnionPlan{},
			wrap:          &physicalMergeSortUnionWrapper{},
			childrenAsSet: true,
		},

		// --- the three CanCorrelate overrides ---
		{
			name:         "FlatMap",
			plan:         &plans.RecordQueryFlatMapPlan{},
			wrap:         &physicalFlatMapWrapper{},
			canCorrelate: true,
		},
		{
			name:         "RecursiveDfsJoin",
			plan:         &plans.RecordQueryRecursiveDfsJoinPlan{},
			wrap:         &physicalRecursiveDfsJoinWrapper{},
			canCorrelate: true,
		},
		{
			name:         "RecursiveLevelUnion",
			plan:         &plans.RecordQueryRecursiveLevelUnionPlan{},
			wrap:         &physicalRecursiveLevelUnionWrapper{},
			canCorrelate: true,
		},

		// --- a sample of the false/false majority, so a spurious override
		// added to either side is caught too, not just a dropped one ---
		{name: "Scan", plan: &plans.RecordQueryScanPlan{}, wrap: &physicalScanWrapper{}},
		{name: "IndexScan", plan: &plans.RecordQueryIndexPlan{}, wrap: &physicalIndexScanWrapper{}},
		{name: "VectorIndexScan", plan: &plans.RecordQueryVectorIndexPlan{}, wrap: &physicalVectorIndexScanWrapper{}},
		{name: "PredicatesFilter", plan: &plans.RecordQueryPredicatesFilterPlan{}, wrap: &physicalPredicatesFilterWrapper{}},
		{name: "TypeFilter", plan: &plans.RecordQueryTypeFilterPlan{}, wrap: &physicalTypeFilterWrapper{}},
		{name: "NestedLoopJoin", plan: &plans.RecordQueryNestedLoopJoinPlan{}, wrap: &physicalNestedLoopJoinWrapper{}},
		{name: "InJoin", plan: &plans.RecordQueryInJoinPlan{}, wrap: &physicalInJoinWrapper{}},
		{name: "InUnion", plan: &plans.RecordQueryInUnionPlan{}, wrap: &physicalInUnionWrapper{}},
		{name: "MultiIntersection", plan: &plans.RecordQueryMultiIntersectionOnValuesPlan{}, wrap: &physicalMultiIntersectionWrapper{}},
		{name: "StreamingAggregation", plan: &plans.RecordQueryStreamingAggregationPlan{}, wrap: &physicalStreamingAggWrapper{}},
		{name: "InMemorySort", plan: &plans.RecordQueryInMemorySortPlan{}, wrap: &physicalInMemorySortWrapper{}},
		{name: "Distinct", plan: &plans.RecordQueryDistinctPlan{}, wrap: &physicalDistinctWrapper{}},
		{name: "Projection", plan: &plans.RecordQueryProjectionPlan{}, wrap: &physicalProjectionWrapper{}},
		// Limit / FetchFromPartialRecord / Map have no physical wrapper since RFC-184
		// W2 — the memo holds the *plans.RecordQuery… plan directly, so their
		// false/false flags are exercised on the live production path (no parity pair
		// to check).
		{name: "DefaultOnEmpty", plan: &plans.RecordQueryDefaultOnEmptyPlan{}, wrap: &physicalDefaultOnEmptyWrapper{}},
		{name: "FirstOrDefault", plan: &plans.RecordQueryFirstOrDefaultPlan{}, wrap: &physicalFirstOrDefaultWrapper{}},
		{name: "Explode", plan: &plans.RecordQueryExplodePlan{}, wrap: &physicalExplodeWrapper{}},
		{name: "TableFunction", plan: &plans.RecordQueryTableFunctionPlan{}, wrap: &physicalTableFunctionWrapper{}},
		{name: "TempTableScan", plan: &plans.RecordQueryTempTableScanPlan{}, wrap: &physicalTempTableScanWrapper{}},
		{name: "TempTableInsert", plan: &plans.RecordQueryTempTableInsertPlan{}, wrap: &physicalTempTableInsertWrapper{}},
		{name: "AggregateIndex", plan: &plans.RecordQueryAggregateIndexPlan{}, wrap: &physicalAggregateIndexWrapper{}},
		{name: "Insert", plan: &plans.RecordQueryInsertPlan{}, wrap: &physicalInsertWrapper{}},
		{name: "Delete", plan: &plans.RecordQueryDeletePlan{}, wrap: &physicalDeleteWrapper{}},
		{name: "Update", plan: &plans.RecordQueryUpdatePlan{}, wrap: &physicalUpdateWrapper{}},
		{name: "Values", plan: &plans.RecordQueryValuesPlan{}, wrap: &physicalValuesWrapper{}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := tc.plan.ChildrenAsSet(); got != tc.childrenAsSet {
				t.Errorf("plan ChildrenAsSet() = %v, want %v", got, tc.childrenAsSet)
			}
			if got := tc.wrap.ChildrenAsSet(); got != tc.childrenAsSet {
				t.Errorf("wrapper ChildrenAsSet() = %v, want %v", got, tc.childrenAsSet)
			}
			if got := tc.plan.CanCorrelate(); got != tc.canCorrelate {
				t.Errorf("plan CanCorrelate() = %v, want %v", got, tc.canCorrelate)
			}
			if got := tc.wrap.CanCorrelate(); got != tc.canCorrelate {
				t.Errorf("wrapper CanCorrelate() = %v, want %v", got, tc.canCorrelate)
			}
		})
	}
}
