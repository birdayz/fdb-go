package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// ImplementSimpleSelectRule implements a SelectExpression with a single
// quantifier (no joins) as a combination of physical plans:
//   - RecordQueryPredicatesFilterPlan for WHERE predicates
//   - RecordQueryMapPlan for non-trivial result values (projections)
//
// Ports Java's ImplementSimpleSelectRule. Only fires on single-quantifier
// SELECTs; multi-quantifier SELECTs (joins) are handled by other rules.
type ImplementSimpleSelectRule struct {
	matcher matching.BindingMatcher
}

func NewImplementSimpleSelectRule() *ImplementSimpleSelectRule {
	return &ImplementSimpleSelectRule{
		matcher: &selectExpressionMatcher{},
	}
}

func (r *ImplementSimpleSelectRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *ImplementSimpleSelectRule) OnMatch(call *ImplementationRuleCall) {
	selectExpr := call.Bindings.Get(r.matcher).(*expressions.SelectExpression)

	quantifiers := selectExpr.GetQuantifiers()
	if len(quantifiers) != 1 {
		return
	}

	innerQuantifier := quantifiers[0]
	innerRef := innerQuantifier.GetRangesOver()
	if innerRef == nil {
		return
	}

	partitions := ToPlanPartitions(innerRef)
	if len(partitions) == 0 {
		return
	}

	resultValue := selectExpr.GetResultValue()
	isSimpleResult := isQuantifiedObjectValueFor(resultValue, innerQuantifier)

	var queryPredicates []predicates.QueryPredicate
	for _, p := range selectExpr.GetPredicates() {
		if !predicates.IsTautology(p) {
			queryPredicates = append(queryPredicates, p)
		}
	}

	for _, partition := range partitions {
		innerExprs := partition.GetExpressions()
		if len(innerExprs) == 0 {
			continue
		}

		needsQuantifierWrap := innerQuantifier.Kind() == expressions.QuantifierExistential ||
			(innerQuantifier.Kind() == expressions.QuantifierForEach && innerQuantifier.IsNullOnEmpty())

		if len(queryPredicates) == 0 && isSimpleResult && !needsQuantifierWrap {
			for _, expr := range innerExprs {
				call.YieldFinalExpression(expr)
			}
			continue
		}

		innerPlans := partition.GetPlans()
		if len(innerPlans) == 0 {
			continue
		}

		currentRef := call.MemoizeFinalExpressionsFromOther(innerRef, innerExprs)
		currentQuant := expressions.NamedPhysicalQuantifier(innerQuantifier.GetAlias(), currentRef)
		currentPlan := innerPlans[0]

		if innerQuantifier.Kind() == expressions.QuantifierExistential {
			fodPlan := plans.NewRecordQueryFirstOrDefaultPlan(currentPlan, values.NewNullValue(values.UnknownType))
			fodWrapper := NewPhysicalFirstOrDefaultWrapper(fodPlan, currentQuant)
			if len(queryPredicates) == 0 && isSimpleResult {
				call.YieldFinalExpression(fodWrapper)
				continue
			}
			fodRef := call.MemoizeFinalExpression(fodWrapper)
			currentQuant = expressions.NewPhysicalQuantifier(fodRef)
			currentPlan = fodPlan
		} else if innerQuantifier.Kind() == expressions.QuantifierForEach && innerQuantifier.IsNullOnEmpty() {
			doePlan := plans.NewRecordQueryDefaultOnEmptyPlan(currentPlan, values.NewNullValue(values.UnknownType))
			doeWrapper := NewPhysicalDefaultOnEmptyWrapper(doePlan, currentQuant)
			if len(queryPredicates) == 0 && isSimpleResult {
				call.YieldFinalExpression(doeWrapper)
				continue
			}
			doeRef := call.MemoizeFinalExpression(doeWrapper)
			currentQuant = expressions.NewPhysicalQuantifier(doeRef)
			currentPlan = doePlan
		}

		if len(queryPredicates) > 0 {
			filterPlan := plans.NewRecordQueryPredicatesFilterPlanWithAlias(currentPlan, queryPredicates, innerQuantifier.GetAlias())
			filterWrapper := NewPhysicalPredicatesFilterWrapper(filterPlan, currentQuant)
			filterRef := call.MemoizeFinalExpression(filterWrapper)
			currentQuant = expressions.NewPhysicalQuantifier(filterRef)
			currentPlan = filterPlan
			if isSimpleResult {
				call.YieldFinalExpression(filterWrapper)
				continue
			}
		}

		if !isSimpleResult {
			mapResultValue := resultValue
			if len(queryPredicates) > 0 {
				mapResultValue = values.RebaseValue(resultValue, values.AliasMap{
					innerQuantifier.GetAlias(): currentQuant.GetAlias(),
				})
			}
			mapPlan := plans.NewRecordQueryMapPlan(currentPlan, mapResultValue)
			mapWrapper := NewPhysicalMapWrapper(mapPlan, currentQuant)
			call.YieldFinalExpression(mapWrapper)
		}
	}
}

func isQuantifiedObjectValueFor(v values.Value, q expressions.Quantifier) bool {
	qov, ok := v.(*values.QuantifiedObjectValue)
	if !ok {
		return false
	}
	return qov.Correlation == q.GetAlias()
}

var _ ImplementationRule = (*ImplementSimpleSelectRule)(nil)

type selectExpressionMatcher struct{}

func (m *selectExpressionMatcher) RootType() string { return "SelectExpression" }

func (m *selectExpressionMatcher) BindMatches(outer *matching.PlannerBindings, in any) []*matching.PlannerBindings {
	if _, ok := in.(*expressions.SelectExpression); !ok {
		return nil
	}
	return []*matching.PlannerBindings{outer.Bind(m, in)}
}
