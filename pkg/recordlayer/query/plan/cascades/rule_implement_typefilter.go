// Portions derived from FoundationDB Record Layer (ImplementTypeFilterRule.java),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package cascades

import (
	"sort"
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// ImplementTypeFilterRule implements a logical LogicalTypeFilterExpression,
// Java's ImplementTypeFilterRule: over each plan partition of the inner that
// produces stored records, a plan whose record types the filter already
// covers is yielded as it is (no type filter needed), and the other plans are
// grouped by the record types the filter keeps from them, each group yielding
// one RecordQueryTypeFilterPlan over a reference restricted to it
// (MemoizeMemberPlansFromOther). OptimizeGroup chooses among the yields; the
// rule pre-selects nothing (RFC-257 WS-F F-8). A plan whose record types
// cannot be stated keeps the filter.
type ImplementTypeFilterRule struct {
	matcher matching.BindingMatcher
}

// NewImplementTypeFilterRule constructs the rule.
func NewImplementTypeFilterRule() *ImplementTypeFilterRule {
	return &ImplementTypeFilterRule{
		matcher: NewExpressionMatcher[*expressions.LogicalTypeFilterExpression]("logical_type_filter"),
	}
}

// Matcher returns the pattern.
func (r *ImplementTypeFilterRule) Matcher() matching.BindingMatcher { return r.matcher }

// OnMatch fires on every LogicalTypeFilterExpression with a
// physical inner.
func (r *ImplementTypeFilterRule) OnMatch(call *ExpressionRuleCall) {
	tf := matching.Get[*expressions.LogicalTypeFilterExpression](call.Bindings, r.matcher)
	innerRef := tf.GetInner().GetRangesOver()
	if innerRef == nil {
		return
	}
	filterTypes := map[string]struct{}{}
	for _, rt := range tf.GetRecordTypes() {
		filterTypes[rt] = struct{}{}
	}
	computeRefPlanProperties(innerRef)
	for _, partition := range ToPlanPartitions(innerRef) {
		if !partition.GetPartitionPropertiesMap().GetBool(properties.PropStoredRecord) {
			continue
		}
		var keys []string
		unsatisfied := map[string][]expressions.RelationalExpression{}
		kept := map[string][]string{}
		for _, member := range partition.GetPhysicalExpressions() {
			childTypes := properties.EvaluateRecordTypes(member)
			covered := len(childTypes) > 0
			var keep []string
			for rt := range childTypes {
				if _, ok := filterTypes[rt]; ok {
					keep = append(keep, rt)
				} else {
					covered = false
				}
			}
			if covered {
				call.Yield(member)
				continue
			}
			if len(childTypes) == 0 {
				keep = append(keep, tf.GetRecordTypes()...)
			}
			sort.Strings(keep)
			key := strings.Join(keep, "\x00")
			if _, seen := unsatisfied[key]; !seen {
				keys = append(keys, key)
				kept[key] = keep
			}
			unsatisfied[key] = append(unsatisfied[key], member)
		}
		for _, key := range keys {
			// The type filter is its own cascades expression (RFC-184 W2): it
			// carries the live child edge directly.
			innerQ := expressions.NewPhysicalQuantifier(call.MemoizeMemberPlansFromOther(innerRef, unsatisfied[key]))
			tfPlan, err := plans.NewRecordQueryTypeFilterPlanFromQuantifier(kept[key], innerQ)
			if err != nil {
				call.Fail(err)
				return
			}
			call.Yield(tfPlan)
		}
	}
}

var _ ExpressionRule = (*ImplementTypeFilterRule)(nil)
