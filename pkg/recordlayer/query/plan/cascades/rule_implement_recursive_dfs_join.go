package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// ImplementRecursiveDfsJoinRule converts a RecursiveUnionExpression
// (where DFS traversal is allowed) into a physical
// RecordQueryRecursiveDfsJoinPlan.
//
// Pattern:
//
//	RecursiveUnion(initial_state, recursive_state)
//	  where dfsAllowed()
//	  → RecursiveDfsJoin(physical(initial), physical(recursive), priorCorrelation, strategy)
//
// The initial-state leg must already have a physical plan (yielded by
// prior TempTableInsert → inner plan implement rules). The recursive
// leg similarly must have a physical plan available.
//
// Mirrors Java's ImplementRecursiveDfsJoinRule.
type ImplementRecursiveDfsJoinRule struct {
	matcher matching.BindingMatcher
}

func NewImplementRecursiveDfsJoinRule() *ImplementRecursiveDfsJoinRule {
	return &ImplementRecursiveDfsJoinRule{
		matcher: NewExpressionMatcher[*expressions.RecursiveUnionExpression]("recursive_union_dfs"),
	}
}

func (r *ImplementRecursiveDfsJoinRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *ImplementRecursiveDfsJoinRule) OnMatch(call *ExpressionRuleCall) {
	recUnion := matching.Get[*expressions.RecursiveUnionExpression](call.Bindings, r.matcher)

	if !recUnion.DfsAllowed() {
		return
	}

	initialRef := recUnion.GetInitialState().GetRangesOver()
	recursiveRef := recUnion.GetRecursiveState().GetRangesOver()
	if initialRef == nil || recursiveRef == nil {
		return
	}

	initialWinner := getWinnerForOrdering(initialRef, properties.PreserveOrdering(), call.CostModel())
	recursiveWinner := getWinnerForOrdering(recursiveRef, properties.PreserveOrdering(), call.CostModel())
	if initialWinner == nil || recursiveWinner == nil {
		return
	}
	initPh, ok := initialWinner.(physicalPlanExpression)
	if !ok {
		return
	}
	recPh, ok := recursiveWinner.(physicalPlanExpression)
	if !ok {
		return
	}

	strategy := plans.DfsPreorder
	if !recUnion.PreOrderAllowed() && recUnion.PostOrderAllowed() {
		strategy = plans.DfsPostorder
	}

	// The prior-value correlation is the temp table scan alias: the
	// recursive leg reads from the temp table that the prior iteration
	// populated.
	priorCorrelation := recUnion.GetTempTableScanAlias()

	// Java's rule (ImplementRecursiveDfsJoinRule) matches
	// tempTableInsertPlanOverQuantifier and builds the DFS plan from the
	// plans UNDER the inserts: the DFS traversal binds the prior row per
	// level (no ping-pong tables), so the level-union's insert tops are
	// dead plumbing here — and under the streaming RecursiveCursor a
	// per-level TempTableInsertCursor continuation would snapshot the
	// accumulator table into EVERY level of the DFS continuation.
	rootPlan := stripTempTableInsertTop(initPh.GetRecordQueryPlan())
	childPlan := stripTempTableInsertTop(recPh.GetRecordQueryPlan())

	var plan *plans.RecordQueryRecursiveDfsJoinPlan
	if recUnion.IsDistinct() {
		plan = plans.NewRecordQueryRecursiveDfsJoinPlanDistinct(
			rootPlan, childPlan,
			priorCorrelation, strategy,
		)
	} else {
		plan = plans.NewRecordQueryRecursiveDfsJoinPlan(
			rootPlan, childPlan,
			priorCorrelation, strategy,
		)
	}

	// Memoize the STRIPPED plans, not the winners they came from.
	//
	// stripTempTableInsertTop removes a TempTableInsert from each leg for the
	// PLAN, but initialWinner/recursiveWinner still carry it. Memoizing those
	// left the quantifier resolving to
	// `TempTableInsert(…, Project(PredicatesFilter(Scan(TREE))))` while the
	// plan child was the bare `Project(PredicatesFilter(Scan(TREE)))` — 58
	// divergent edges across the corpus (RFC-183 §12). The memo was costing a
	// leg with plumbing the executed plan does not have.
	//
	// Note this is the INVERSE of the FlatMap sites in
	// rule_implement_nested_loop_join.go, where the plan held compensating
	// filters the quantifier lacked. Same defect class — plan and quantifier
	// describing different expressions — reached from opposite directions,
	// which is why there is no single mechanical rewrite for §12's remaining
	// sites.
	//
	// scanPlanExpression is the existing plan-backed adapter for exactly this
	// (see its other use for the buried-leg rebase): it makes the memoized
	// expression report the plan actually being executed.
	// MemoizeFinalExpression, NOT MemoizeExpression — the two legs must land
	// in SEPARATE references.
	//
	// MemoizeExpression interns, and scanPlanExpression is indistinguishable
	// to the memo when two plans share a root node: it hashes and compares via
	// EqualsPlanWithoutChildren (children excluded) and reports NO quantifiers,
	// so the memo has nothing left to tell two of them apart. Both legs here
	// are RecordQueryProjectionPlan with the same projections — root is
	// Project([ID,PARENT], TypeFilter(Scan)), child is Project([ID,PARENT],
	// Project(FlatMap(…))) — so they compare EQUAL and intern into ONE group.
	// rootQ and childQ then range over the same single-member reference, and
	// the memo believes the two legs are the same expression.
	//
	// That collapse was introduced by an earlier revision of this very fix and
	// is why the leg divergence only fell from 58 to 25 rather than to zero
	// (RFC-183 §14). It stayed invisible to tests and to the corpus differ
	// because WithChildren keeps `plan: w.plan`, so EXTRACTION reads the plan
	// and never notices — the damage is confined to costing and bookkeeping,
	// which is the exact defect this fix exists to remove.
	rootQ := expressions.ForEachQuantifier(call.MemoizeFinalExpression(&scanPlanExpression{plan: rootPlan}))
	childQ := expressions.ForEachQuantifier(call.MemoizeFinalExpression(&scanPlanExpression{plan: childPlan}))
	call.Yield(newPhysicalRecursiveDfsJoinWrapper(plan, rootQ, childQ))
}

// stripTempTableInsertTop unwraps a TempTableInsert at the top of a leg plan
// (Java's initialInnerPlanMatcher / recursive TempTableInsertExpression
// matcher shapes take the plan UNDER the insert). Match-surface divergence:
// Java's matcher REQUIRES the insert top (the rule does not fire without
// it), while this strip tolerates its absence — benign because the front
// end always builds recursive legs insert-topped, and a hypothetical bare
// leg would plan identically rather than silently mis-fire.
func stripTempTableInsertTop(p plans.RecordQueryPlan) plans.RecordQueryPlan {
	if ins, ok := p.(*plans.RecordQueryTempTableInsertPlan); ok {
		return ins.GetInner()
	}
	return p
}

var _ ExpressionRule = (*ImplementRecursiveDfsJoinRule)(nil)
