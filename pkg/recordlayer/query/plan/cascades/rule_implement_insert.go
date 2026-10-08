package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// ImplementInsertRule implements a logical InsertExpression as a physical
// RecordQueryInsertPlan, Java's ImplementInsertRule: one INSERT plan per plan
// partition of the inner, ranging over a reference restricted to that
// partition's plans (MemoizeMemberPlansFromOther). OptimizeGroup chooses among
// the yields; the rule pre-selects nothing (RFC-257 WS-F F-8).
type ImplementInsertRule struct {
	matcher matching.BindingMatcher
}

// NewImplementInsertRule constructs the rule.
func NewImplementInsertRule() *ImplementInsertRule {
	return &ImplementInsertRule{
		matcher: NewExpressionMatcher[*expressions.InsertExpression]("insert"),
	}
}

// Matcher returns the pattern.
func (r *ImplementInsertRule) Matcher() matching.BindingMatcher { return r.matcher }

// OnMatch fires on every InsertExpression with a physical inner.
func (r *ImplementInsertRule) OnMatch(call *ExpressionRuleCall) {
	ins := matching.Get[*expressions.InsertExpression](call.Bindings, r.matcher)
	innerRef := ins.GetInner().GetRangesOver()
	if innerRef == nil {
		return
	}
	computeRefPlanProperties(innerRef)
	for _, partition := range ToPlanPartitions(innerRef) {
		members := partition.GetPhysicalExpressions()
		if len(members) == 0 {
			continue
		}
		// The INSERT plan is its own cascades expression (RFC-184 W2): it
		// carries the live child edge directly. The edge keeps the logical
		// alias (Java's physicalBuilder().morphFrom(innerQuantifier)).
		innerQ := expressions.NamedPhysicalQuantifier(ins.GetInner().GetAlias(),
			call.MemoizeMemberPlansFromOther(innerRef, members))
		insPlan, err := plans.NewRecordQueryInsertPlanFromQuantifier(innerQ, ins.GetTargetRecordType(), ins.GetTargetType())
		if err != nil {
			call.Fail(err)
			return
		}
		call.Yield(insPlan)
	}
}

var _ ExpressionRule = (*ImplementInsertRule)(nil)
