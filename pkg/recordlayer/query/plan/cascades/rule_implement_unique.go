package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
)

// ImplementUniqueRule implements LogicalUniqueExpression by absorbing
// it when the inner Reference's plans are already distinct (with a
// primary key). If the inner plans produce distinct records, the
// Unique operator is a no-op and we yield the inner plans directly.
//
// Ports Java's ImplementUniqueRule.
type ImplementUniqueRule struct {
	matcher matching.BindingMatcher
}

func NewImplementUniqueRule() *ImplementUniqueRule {
	return &ImplementUniqueRule{
		matcher: &logicalUniqueMatcher{},
	}
}

func (r *ImplementUniqueRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *ImplementUniqueRule) OnMatch(call *ImplementationRuleCall) {
	expr := call.Bindings.Get(r.matcher).(*expressions.LogicalUniqueExpression)

	innerRef := expr.GetInner().GetRangesOver()
	if innerRef == nil {
		return
	}

	partitions := ToPlanPartitions(innerRef)

	var filtered []*PlanPartition
	for _, p := range partitions {
		if p.IsDistinct() {
			filtered = append(filtered, p)
		}
	}

	rolled := RollUpPlanPartitions(filtered)

	for _, partition := range rolled {
		for _, wrapperExpr := range partition.GetExpressions() {
			call.YieldFinalExpression(wrapperExpr)
		}
	}
}

var _ ImplementationRule = (*ImplementUniqueRule)(nil)

type logicalUniqueMatcher struct{}

func (m *logicalUniqueMatcher) RootType() string { return "LogicalUniqueExpression" }

func (m *logicalUniqueMatcher) BindMatches(outer *matching.PlannerBindings, in any) []*matching.PlannerBindings {
	if _, ok := in.(*expressions.LogicalUniqueExpression); !ok {
		return nil
	}
	return []*matching.PlannerBindings{outer.Bind(m, in)}
}
