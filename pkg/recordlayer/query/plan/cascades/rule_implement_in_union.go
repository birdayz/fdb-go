package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// ImplementInUnionRule implements a SELECT over ExplodeExpressions
// as a RecordQueryInUnionPlan — the inner plan is executed once per
// IN value and results are merge-sorted by comparison keys.
//
// Ports Java's ImplementInUnionRule. Same ordering-aware gap as
// ImplementInJoinRule — the rule creates InUnion plans without
// ordering-aware binding adjustment. Java's full version adjusts
// bindings that reference explode aliases to promote them from
// fixed to directional sort orders, enabling merge-sorted output.
// This requires Comparison.getCorrelatedTo() correlation tracking.
type ImplementInUnionRule struct {
	matcher matching.BindingMatcher
}

func NewImplementInUnionRule() *ImplementInUnionRule {
	return &ImplementInUnionRule{
		matcher: &selectExpressionMatcher{},
	}
}

func (r *ImplementInUnionRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *ImplementInUnionRule) OnMatch(call *ImplementationRuleCall) {
	selectExpr := call.Bindings.Get(r.matcher).(*expressions.SelectExpression)

	if selectExpr.HasPredicates() {
		return
	}

	quantifiers := selectExpr.GetQuantifiers()
	if len(quantifiers) < 2 {
		return
	}

	resultValue := selectExpr.GetResultValue()

	var explodeQuantifiers []expressions.Quantifier
	var innerQuantifier expressions.Quantifier
	hasInner := false

	for _, q := range quantifiers {
		ref := q.GetRangesOver()
		if ref == nil {
			return
		}
		if isExplodeExpression(ref) {
			explodeQuantifiers = append(explodeQuantifiers, q)
		} else if !hasInner {
			innerQuantifier = q
			hasInner = true
		} else {
			return
		}
	}

	if !hasInner || len(explodeQuantifiers) == 0 {
		return
	}

	qov, ok := resultValue.(*values.QuantifiedObjectValue)
	if !ok || qov.Correlation != innerQuantifier.GetAlias() {
		return
	}

	innerRef := innerQuantifier.GetRangesOver()
	if innerRef == nil {
		return
	}

	partitions := ToPlanPartitions(innerRef)
	if len(partitions) == 0 {
		return
	}

	bindingNames := make([]string, len(explodeQuantifiers))
	for i, eq := range explodeQuantifiers {
		bindingNames[i] = eq.GetAlias().String()
	}

	for _, partition := range partitions {
		innerPlans := partition.GetPlans()
		if len(innerPlans) == 0 {
			continue
		}

		innerExprs := partition.GetExpressions()
		newRef := call.MemoizeFinalExpressionsFromOther(innerRef, innerExprs)

		inUnionPlan := plans.NewRecordQueryInUnionPlan(
			innerPlans[0], bindingNames, nil, false)
		call.YieldFinalExpression(NewPhysicalInUnionWrapper(
			inUnionPlan,
			expressions.NewPhysicalQuantifier(newRef),
		))
	}
}

var _ ImplementationRule = (*ImplementInUnionRule)(nil)
