package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// ImplementInJoinRule implements a SELECT over ExplodeExpressions
// (UNNEST of IN-lists) and a correlated inner plan as a right-deep
// chain of RecordQueryInJoinPlans. Each explode becomes one IN-source;
// the inner plan is executed once per combination of IN values.
//
// Ports Java's ImplementInJoinRule. The rule creates InJoin plans in
// quantifier order (last explode = innermost). Java's full version
// does ordering-aware source selection by matching explode aliases to
// the inner plan's ordering bindings (via Ordering.Binding's
// comparison correlation tracking) — that requires the full
// Comparison.getCorrelatedTo() infrastructure which is not yet ported.
// The current behavior is functionally correct (produces correct
// results) but doesn't exploit ordering for optimal IN-source nesting.
type ImplementInJoinRule struct {
	matcher matching.BindingMatcher
}

func NewImplementInJoinRule() *ImplementInJoinRule {
	return &ImplementInJoinRule{
		matcher: &selectExpressionMatcher{},
	}
}

func (r *ImplementInJoinRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *ImplementInJoinRule) OnMatch(call *ImplementationRuleCall) {
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

	for _, partition := range partitions {
		innerPlans := partition.GetPlans()
		if len(innerPlans) == 0 {
			continue
		}

		innerExprs := partition.GetExpressions()
		currentRef := call.MemoizeFinalExpressionsFromOther(innerRef, innerExprs)

		for i := len(explodeQuantifiers) - 1; i >= 0; i-- {
			eq := explodeQuantifiers[i]
			bindingName := eq.GetAlias().String()
			inJoinPlan := plans.NewRecordQueryInJoinPlan(
				innerPlans[0], bindingName, false, false)
			wrapper := NewPhysicalInJoinWrapper(inJoinPlan,
				expressions.NewPhysicalQuantifier(currentRef))
			currentRef = call.MemoizeFinalExpression(wrapper)
		}

		for _, m := range currentRef.FinalMembers() {
			call.YieldFinalExpression(m)
		}
	}
}

func isExplodeExpression(ref *expressions.Reference) bool {
	for _, m := range ref.AllMembers() {
		if _, ok := m.(*expressions.ExplodeExpression); ok {
			return true
		}
	}
	return false
}

var _ ImplementationRule = (*ImplementInJoinRule)(nil)
