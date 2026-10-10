// Portions derived from FoundationDB Record Layer (
// ImplementTempTableInsertRule.java, TempTableInsertPlan.java),
// Copyright 2015-2019 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2025 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// ImplementTempTableInsertRule converts a TempTableInsertExpression
// to a physical TempTableInsertPlan, Java's ImplementTempTableInsertRule: one
// plan per plan partition of the inner, ranging over a reference restricted to
// that partition's plans (MemoizeMemberPlansFromOther). The rule pre-selects
// nothing (RFC-257 WS-F F-8).
type ImplementTempTableInsertRule struct {
	matcher matching.BindingMatcher
}

func NewImplementTempTableInsertRule() *ImplementTempTableInsertRule {
	return &ImplementTempTableInsertRule{
		matcher: NewExpressionMatcher[*expressions.TempTableInsertExpression]("temp_table_insert"),
	}
}

func (r *ImplementTempTableInsertRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *ImplementTempTableInsertRule) OnMatch(call *ExpressionRuleCall) {
	insert := matching.Get[*expressions.TempTableInsertExpression](call.Bindings, r.matcher)

	innerRef := insert.GetInner().GetRangesOver()
	if innerRef == nil {
		return
	}
	computeRefPlanProperties(innerRef)
	for _, partition := range ToPlanPartitions(innerRef) {
		members := partition.GetPhysicalExpressions()
		if len(members) == 0 {
			continue
		}
		// Build the insert over the SAME live memo edge it reports as its
		// child. The plan IS the cascades expression the memo holds (RFC-184 W2).
		innerQ := expressions.NewPhysicalQuantifier(call.MemoizeMemberPlansFromOther(innerRef, members))
		plan, err := plans.NewRecordQueryTempTableInsertPlanFromQuantifier(
			innerQ,
			insert.GetTempTableAlias(),
			insert.IsOwning(),
		)
		if err != nil {
			call.Fail(err)
			return
		}
		call.Yield(plan)
	}
}

var _ ExpressionRule = (*ImplementTempTableInsertRule)(nil)
