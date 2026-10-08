package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
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
// Mirrors Java's ImplementRecursiveDfsJoinRule, which pre-selects nothing
// (RFC-257 WS-F F-8): one DFS plan per final plan under each initial-state
// insert, its root ranging over that one plan, and the recursive leg ranging
// over every plan of its rolled-up partition.
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
	//
	// The legs range over references restricted to plans UNDER the inserts,
	// so the memo costs exactly the plans executed, never the plumbing
	// (RFC-183 §12), and each leg is its own reference.
	var roots []dfsJoinLeg
	for _, leg := range dfsJoinLegsUnderInserts(initialRef) {
		for _, member := range leg.members {
			roots = append(roots, dfsJoinLeg{source: leg.source, members: []expressions.RelationalExpression{member}})
		}
	}
	for _, root := range roots {
		for _, child := range dfsJoinLegsUnderInserts(recursiveRef) {
			rootQ := expressions.NewPhysicalQuantifier(call.MemoizeMemberPlansFromOther(root.source, root.members))
			childQ := expressions.NewPhysicalQuantifier(call.MemoizeMemberPlansFromOther(child.source, child.members))
			// The plan carries its two leg edges directly — no separate
			// physical wrapper (RFC-184 W2).
			plan, err := plans.NewRecordQueryRecursiveDfsJoinPlanFromQuantifiers(
				rootQ, childQ, priorCorrelation, strategy, recUnion.IsDistinct(),
			)
			if err != nil {
				call.Fail(err)
				return
			}
			call.Yield(plan)
		}
	}
}

// dfsJoinLeg is a set of plans one DFS-join leg may range over, all members of
// source.
type dfsJoinLeg struct {
	source  *expressions.Reference
	members []expressions.RelationalExpression
}

// dfsJoinLegsUnderInserts returns, for each physical plan of ref's rolled-up
// partition, the plans the DFS join ranges over in its place: every plan of
// the rolled-up partition under a TempTableInsert top (Java matches
// tempTableInsertPlanOverQuantifier and takes the plans below it), or the
// plan itself for a leg without one. Match-surface divergence: Java's matcher
// REQUIRES the insert top (the rule does not fire without it), while Go
// tolerates its absence — benign because the front end always builds
// recursive legs insert-topped, and a hypothetical bare leg would plan
// identically rather than silently mis-fire.
func dfsJoinLegsUnderInserts(ref *expressions.Reference) []dfsJoinLeg {
	var out []dfsJoinLeg
	for _, member := range rolledUpPhysicalMembers(ref) {
		insert, ok := member.(*plans.RecordQueryTempTableInsertPlan)
		if !ok {
			out = append(out, dfsJoinLeg{source: ref, members: []expressions.RelationalExpression{member}})
			continue
		}
		quantifiers := insert.GetQuantifiers()
		if len(quantifiers) != 1 || quantifiers[0].GetRangesOver() == nil {
			continue
		}
		below := quantifiers[0].GetRangesOver()
		if members := rolledUpPhysicalMembers(below); len(members) > 0 {
			out = append(out, dfsJoinLeg{source: below, members: members})
		}
	}
	return out
}

var _ ExpressionRule = (*ImplementRecursiveDfsJoinRule)(nil)
