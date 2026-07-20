package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
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
		matcher: NewExpressionMatcher[*expressions.SelectExpression]("implement_simple_select"),
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
	// The outer MAP is skipped ONLY when the result value is an EXACT passthrough
	// of the inner quantifier — a QOV over its alias whose result TYPE equals the
	// flowed object type. A passthrough that merely WIDENS the nullability keeps
	// the MAP so the type widening is expressed at runtime (Java's
	// ImplementSimpleSelectRule isSimpleResultValue, RFC-144 BC2). Keying the skip
	// on passthrough alone (the prior behaviour) would drop the widening MAP.
	isSimpleResult := isSimplePassthroughOf(resultValue, innerQuantifier)

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
			// FirstOrDefault stays wrapped (RFC-184 W2): its collapse is NOT
			// identity-neutral — in the correlated EXISTS/scalar-subquery path the
			// FOD wraps a SARG-pushed snapshot (currentPlan) that deliberately
			// diverges from the live memo edge, so a FromQuantifier build re-resolves
			// to the non-SARG base and shifts the plan (flatmap_exists corpus). It
			// needs the deferred-winner rework as its own change.
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
			// The DefaultOnEmpty is its own cascades expression carrying the live
			// currentQuant edge (RFC-184 W2) — no physicalDefaultOnEmptyWrapper.
			doePlan := plans.NewRecordQueryDefaultOnEmptyPlanFromQuantifier(currentQuant, values.NewNullValue(values.UnknownType))
			if len(queryPredicates) == 0 && isSimpleResult {
				call.YieldFinalExpression(doePlan)
				continue
			}
			doeRef := call.MemoizeFinalExpression(doePlan)
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
			// The projection (Map) is its own cascades expression carrying the
			// live currentQuant edge (RFC-184 W2).
			mapPlan := plans.NewRecordQueryMapPlanFromQuantifier(currentQuant, mapResultValue)
			call.YieldFinalExpression(mapPlan)
		}
	}
}

// isSimplePassthroughOf reports whether v is an EXACT passthrough of quantifier
// q: a QuantifiedObjectValue over q's alias whose result TYPE equals q's flowed
// object type. A passthrough whose type only WIDENS the nullability is NOT
// simple — the MAP must be kept to express the widening (Java's
// QuantifiedObjectValue.isSimpleQuantifiedObjectValueOver + the
// resultType.equals(flowedType) gate; RFC-144 BC2).
func isSimplePassthroughOf(v values.Value, q expressions.Quantifier) bool {
	qov, ok := v.(*values.QuantifiedObjectValue)
	if !ok {
		return false
	}
	if qov.Correlation != q.GetAlias() {
		return false
	}
	flowedType := q.GetFlowedObjectValue().Type()
	return v.Type().Equals(flowedType)
}

var _ ImplementationRule = (*ImplementSimpleSelectRule)(nil)
