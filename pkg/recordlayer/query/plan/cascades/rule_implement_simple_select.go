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
	// A strict edge is meaningful only as the inner of the binary correlated
	// scalar FlatMap. Implementing it as a standalone passthrough/filter would
	// erase the at-most-one-row check.
	if hasStrictSingleQuantifier(quantifiers) {
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
			// FirstOrDefault collapses onto a DISENTANGLED FINAL edge holding the
			// concrete SARG-pushed member currentPlan (constraint-preserving
			// disentangle, RFC-184 W2). The correlated EXISTS/scalar-subquery FOD
			// wraps a SARG-pushed snapshot that deliberately diverges from the shared
			// group's winner; freezing currentPlan into a PRIVATE single-member
			// reference makes planFromQuantifier resolve the SARG member — never the
			// non-SARG base the shared multi-member group currentRef would float to.
			// The frozen edge keeps innerQuantifier's alias so GetResultValue is
			// unchanged.
			fodInnerQ := expressions.NamedPhysicalQuantifier(innerQuantifier.GetAlias(),
				call.MemoizeFinalExpression(currentPlan))
			fodPlan := plans.NewRecordQueryFirstOrDefaultPlanFromQuantifier(fodInnerQ, values.NewNullValue(values.UnknownType))
			if len(queryPredicates) == 0 && isSimpleResult {
				call.YieldFinalExpression(fodPlan)
				continue
			}
			fodRef := call.MemoizeFinalExpression(fodPlan)
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
			// The filter carries the LIVE currentQuant edge over the shared inner
			// group (RFC-184 W2) — exactly the edge the wrapper's innerQuant
			// presented. A plain filter's inner has no correlated SARG snapshot to
			// preserve (unlike FoD/NLJ), so ranging over the live group keeps
			// push_filter_through_fetch's re-explored pushed member reachable from a
			// parent that captures this leg; a frozen snapshot strands the pre-push
			// filter once the merged group canonicalizes to the pushed one.
			filterPlan := plans.NewRecordQueryPredicatesFilterPlanWithAliasFromQuantifier(currentQuant, queryPredicates, innerQuantifier.GetAlias())
			filterRef := call.MemoizeFinalExpression(filterPlan)
			currentQuant = expressions.NewPhysicalQuantifier(filterRef)
			currentPlan = filterPlan
			if isSimpleResult {
				call.YieldFinalExpression(filterPlan)
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
