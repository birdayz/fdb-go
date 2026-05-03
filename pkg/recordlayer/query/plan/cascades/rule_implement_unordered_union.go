package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// ImplementUnorderedUnionRule implements LogicalUnionExpression as a
// RecordQueryUnorderedUnionPlan. It extracts physical plans from each
// child Reference's plan partitions and creates a concatenating union
// plan over them.
//
// Ports Java's ImplementUnorderedUnionRule.
type ImplementUnorderedUnionRule struct {
	matcher matching.BindingMatcher
}

func NewImplementUnorderedUnionRule() *ImplementUnorderedUnionRule {
	return &ImplementUnorderedUnionRule{
		matcher: &logicalUnionMatcher{},
	}
}

func (r *ImplementUnorderedUnionRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *ImplementUnorderedUnionRule) OnMatch(call *ImplementationRuleCall) {
	expr := call.Bindings.Get(r.matcher).(*expressions.LogicalUnionExpression)

	quantifiers := expr.GetQuantifiers()
	if len(quantifiers) == 0 {
		return
	}

	childPartitions := make([][]*PlanPartition, len(quantifiers))
	for i, q := range quantifiers {
		ref := q.GetRangesOver()
		if ref == nil {
			return
		}
		parts := ToPlanPartitions(ref)
		rolled := RollUpPlanPartitions(parts)
		if len(rolled) == 0 {
			return
		}
		childPartitions[i] = rolled
	}

	for _, partitions := range crossProductPartitions(childPartitions) {
		var childPlans []plans.RecordQueryPlan
		var newQuantifiers []expressions.Quantifier

		for i, partition := range partitions {
			planExprs := partition.GetExpressions()
			if len(planExprs) == 0 {
				continue
			}

			newRef := call.MemoizeFinalExpressionsFromOther(
				quantifiers[i].GetRangesOver(),
				planExprs,
			)
			newQuantifiers = append(newQuantifiers,
				expressions.NewPhysicalQuantifier(newRef))

			for _, pe := range planExprs {
				if ph, ok := pe.(physicalPlanExpression); ok {
					childPlans = append(childPlans, ph.GetRecordQueryPlan())
				}
			}
		}

		if len(childPlans) < 2 {
			continue
		}

		unionPlan := plans.NewRecordQueryUnorderedUnionPlan(childPlans)
		wrapper := NewPhysicalUnorderedUnionWrapper(unionPlan, newQuantifiers)
		call.YieldFinalExpression(wrapper)
	}
}

var _ ImplementationRule = (*ImplementUnorderedUnionRule)(nil)

type logicalUnionMatcher struct{}

func (m *logicalUnionMatcher) RootType() string { return "LogicalUnionExpression" }

func (m *logicalUnionMatcher) BindMatches(outer *matching.PlannerBindings, in any) []*matching.PlannerBindings {
	if _, ok := in.(*expressions.LogicalUnionExpression); !ok {
		return nil
	}
	return []*matching.PlannerBindings{outer.Bind(m, in)}
}

// crossProductPartitions returns the Cartesian product of per-child
// partition lists. Delegates to the generic CrossProduct.
func crossProductPartitions(childPartitions [][]*PlanPartition) [][]*PlanPartition {
	return CrossProduct(childPartitions)
}
