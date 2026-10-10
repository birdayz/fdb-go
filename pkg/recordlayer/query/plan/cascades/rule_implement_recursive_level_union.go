// Portions derived from FoundationDB Record Layer (
// ImplementRecursiveLevelUnionRule.java, RelationalExpression.java),
// Copyright 2015-2025 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2026 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// ImplementRecursiveLevelUnionRule converts a RecursiveUnionExpression
// (where level-order traversal is allowed) into a physical
// RecordQueryRecursiveLevelUnionPlan.
//
// Pattern:
//
//	RecursiveUnion(initial_state, recursive_state)
//	  where levelAllowed()
//	  → RecursiveLevelUnion(physical(initial), physical(recursive), scanAlias, insertAlias)
//
// Both legs must already have physical plans available (yielded by
// prior TempTableInsert → inner plan implement rules).
//
// Mirrors Java's ImplementRecursiveLevelUnionRule: each leg ranges over a
// reference holding every plan of its rolled-up plan partition
// (memoizeMemberPlansFromOther), so the rule pre-selects nothing and the legs'
// own optimization chooses (RFC-257 WS-F F-8).
type ImplementRecursiveLevelUnionRule struct {
	matcher matching.BindingMatcher
}

func NewImplementRecursiveLevelUnionRule() *ImplementRecursiveLevelUnionRule {
	return &ImplementRecursiveLevelUnionRule{
		matcher: NewExpressionMatcher[*expressions.RecursiveUnionExpression]("recursive_union_level"),
	}
}

func (r *ImplementRecursiveLevelUnionRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *ImplementRecursiveLevelUnionRule) OnMatch(call *ExpressionRuleCall) {
	recUnion := matching.Get[*expressions.RecursiveUnionExpression](call.Bindings, r.matcher)

	if !recUnion.LevelAllowed() {
		return
	}

	initialRef := recUnion.GetInitialState().GetRangesOver()
	recursiveRef := recUnion.GetRecursiveState().GetRangesOver()
	if initialRef == nil || recursiveRef == nil {
		return
	}

	initialPlans := rolledUpPhysicalMembers(initialRef)
	recursivePlans := rolledUpPhysicalMembers(recursiveRef)
	if len(initialPlans) == 0 || len(recursivePlans) == 0 {
		return
	}

	// The plan carries its two leg edges directly — one live quantifier per
	// leg, no separate physical wrapper (RFC-184 W2).
	initQ := expressions.NewPhysicalQuantifier(call.MemoizeMemberPlansFromOther(initialRef, initialPlans))
	recQ := expressions.NewPhysicalQuantifier(call.MemoizeMemberPlansFromOther(recursiveRef, recursivePlans))
	plan, err := plans.NewRecordQueryRecursiveLevelUnionPlanFromQuantifiers(
		initQ, recQ,
		recUnion.GetTempTableScanAlias(),
		recUnion.GetTempTableInsertAlias(),
		recUnion.IsDistinct(),
	)
	if err != nil {
		call.Fail(err)
		return
	}
	call.Yield(plan)
}

// rolledUpPhysicalMembers returns the physical plans of ref's plan partitions
// rolled up into one (Java's planPartitions(rollUpPartitions(any(...)))).
func rolledUpPhysicalMembers(ref *expressions.Reference) []expressions.RelationalExpression {
	computeRefPlanProperties(ref)
	var out []expressions.RelationalExpression
	for _, partition := range RollUpPlanPartitions(ToPlanPartitions(ref)) {
		out = append(out, partition.GetPhysicalExpressions()...)
	}
	return out
}

var _ ExpressionRule = (*ImplementRecursiveLevelUnionRule)(nil)
