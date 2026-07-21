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
			// Union lost its wrapper (RFC-184 W2) but keeps the non-default
			// ChildrenAsSet=true override, so the plan side stays pinned here
			// with no wrapper pair (wrap nil → wrapper checks skip).
			name:          "Union",
			plan:          &plans.RecordQueryUnionPlan{},
			childrenAsSet: true,
		},
		{
			// UnorderedUnion lost its wrapper (RFC-184 W2) but keeps the
			// non-default ChildrenAsSet=true, so the plan side stays pinned with
			// no wrapper pair (wrap nil → wrapper checks skip).
			name:          "UnorderedUnion",
			plan:          &plans.RecordQueryUnorderedUnionPlan{},
			childrenAsSet: true,
		},
		{
			name:          "Intersection",
			plan:          &plans.RecordQueryIntersectionPlan{},
			childrenAsSet: true,
		},
		{
			name:          "MergeSortUnion",
			plan:          &plans.RecordQueryMergeSortUnionPlan{},
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
			canCorrelate: true,
		},
		{
			name:         "RecursiveLevelUnion",
			plan:         &plans.RecordQueryRecursiveLevelUnionPlan{},
			canCorrelate: true,
		},

		// --- a sample of the false/false majority, so a spurious override
		// added to either side is caught too, not just a dropped one ---
		// Scan lost its wrapper (RFC-184 W2) but keeps the false/false default,
		// so the plan side stays pinned here with no wrapper pair.
		{name: "Scan", plan: &plans.RecordQueryScanPlan{}},
		// PredicatesFilter lost its wrapper (RFC-184 W2) but keeps the false/false
		// default, so the plan side stays pinned here with no wrapper pair.
		{name: "PredicatesFilter", plan: &plans.RecordQueryPredicatesFilterPlan{}},
		{name: "NestedLoopJoin", plan: &plans.RecordQueryNestedLoopJoinPlan{}, wrap: &physicalNestedLoopJoinWrapper{}},
		// InJoin and InUnion lost their wrappers (RFC-184 W2) but keep the
		// false/false default, so the plan side stays pinned here with no wrapper
		// pair.
		{name: "InJoin", plan: &plans.RecordQueryInJoinPlan{}},
		{name: "InUnion", plan: &plans.RecordQueryInUnionPlan{}},
		{name: "MultiIntersection", plan: &plans.RecordQueryMultiIntersectionOnValuesPlan{}},
		// StreamingAggregation and Distinct lost their wrappers (RFC-184 W2) but
		// keep the false/false default, so the plan side stays pinned here with no
		// wrapper pair.
		{name: "StreamingAggregation", plan: &plans.RecordQueryStreamingAggregationPlan{}},
		{name: "InMemorySort", plan: &plans.RecordQueryInMemorySortPlan{}, wrap: &physicalInMemorySortWrapper{}},
		// FirstOrDefault and Distinct lost their wrappers (RFC-184 W2) but keep the
		// false/false default, so the plan side stays pinned here with no wrapper
		// pair.
		{name: "FirstOrDefault", plan: &plans.RecordQueryFirstOrDefaultPlan{}},
		{name: "Distinct", plan: &plans.RecordQueryDistinctPlan{}},
		// IndexScan / Limit / FetchFromPartialRecord / Map / DefaultOnEmpty / Explode /
		// TableFunction / TempTableScan / TempTableInsert / VectorIndexScan / Values /
		// AggregateIndex / TypeFilter / Insert / Delete / Update have no physical
		// wrapper since RFC-184 W2 — the memo holds the *plans.RecordQuery… plan
		// directly, so their false/false flags are exercised on the live production
		// path (no parity pair to check). Only InMemorySort KEEPS its wrapper
		// (collapse deferred — the last ordering-critical producer wrapper).
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := tc.plan.ChildrenAsSet(); got != tc.childrenAsSet {
				t.Errorf("plan ChildrenAsSet() = %v, want %v", got, tc.childrenAsSet)
			}
			if got := tc.plan.CanCorrelate(); got != tc.canCorrelate {
				t.Errorf("plan CanCorrelate() = %v, want %v", got, tc.canCorrelate)
			}
			// The wrapper side is checked only while a wrapper still exists for
			// this plan type; RFC-184 W2 collapses retire wrappers, leaving the
			// plan the live production path (wrap==nil skips the pair check).
			if tc.wrap == nil {
				return
			}
			if got := tc.wrap.ChildrenAsSet(); got != tc.childrenAsSet {
				t.Errorf("wrapper ChildrenAsSet() = %v, want %v", got, tc.childrenAsSet)
			}
			if got := tc.wrap.CanCorrelate(); got != tc.canCorrelate {
				t.Errorf("wrapper CanCorrelate() = %v, want %v", got, tc.canCorrelate)
			}
		})
	}
}
