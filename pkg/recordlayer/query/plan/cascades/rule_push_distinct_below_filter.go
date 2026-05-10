package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// PushDistinctBelowFilterRule moves a physical
// RecordQueryUnorderedPrimaryKeyDistinctPlan below a
// RecordQueryPredicatesFilterPlan. This ensures generated plans match
// the form produced by Java's heuristic planner (distinct below filter).
//
// Pattern:
//
//	Distinct(Filter([P], inner))  →  Filter([P'], Distinct(inner))
//
// Predicates P are rebased from the old filter quantifier alias to the
// new quantifier over the distinct plan.
//
// Mirrors Java's `PushDistinctBelowFilterRule`.
type PushDistinctBelowFilterRule struct {
	matcher matching.BindingMatcher
}

func NewPushDistinctBelowFilterRule() *PushDistinctBelowFilterRule {
	return &PushDistinctBelowFilterRule{
		matcher: NewExpressionMatcher[*physicalDistinctWrapper]("phys_distinct"),
	}
}

func (r *PushDistinctBelowFilterRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *PushDistinctBelowFilterRule) OnMatch(call *ImplementationRuleCall) {
	distinctW := matching.Get[*physicalDistinctWrapper](call.Bindings, r.matcher)

	innerRef := distinctW.innerQuant.GetRangesOver()
	if innerRef == nil {
		return
	}

	// Find a physical filter wrapper in the distinct's inner.
	var filterW *physicalPredicatesFilterWrapper
	for _, m := range innerRef.AllMembers() {
		if fw, ok := m.(*physicalPredicatesFilterWrapper); ok {
			filterW = fw
			break
		}
	}
	if filterW == nil {
		return
	}

	// Get the filter's inner reference.
	filterInnerRef := filterW.innerQuant.GetRangesOver()
	if filterInnerRef == nil {
		return
	}

	// Build: Distinct(filterInner)
	filterInnerExpr := findPhysicalExpr(filterInnerRef)
	if filterInnerExpr == nil {
		return
	}

	newDistinctPlan := plans.NewRecordQueryDistinctPlan(nil)
	newDistinctQ := expressions.ForEachQuantifier(
		call.MemoizeFinalExpressionsFromOther(filterInnerRef, []expressions.RelationalExpression{filterInnerExpr}),
	)
	newDistinctWrapper := NewPhysicalDistinctWrapper(newDistinctPlan, newDistinctQ)

	// Memoize the new distinct wrapper.
	distinctRef := call.MemoizeFinalExpression(newDistinctWrapper)

	// Create new quantifier over the distinct plan.
	newQOverDistinct := expressions.ForEachQuantifier(distinctRef)

	// Rebase predicates: translate from old filter's inner alias to new quantifier alias.
	oldAlias := filterW.innerQuant.GetAlias()
	newAlias := newQOverDistinct.GetAlias()
	rebasedPreds := rebasePredicates(filterW.plan.GetPredicates(), oldAlias, newAlias)

	// Build: Filter([P'], Distinct(inner))
	newFilterPlan := plans.NewRecordQueryPredicatesFilterPlan(nil, rebasedPreds)
	newFilterWrapper := NewPhysicalPredicatesFilterWrapper(newFilterPlan, newQOverDistinct)

	call.Yield(newFilterWrapper)
}

// rebasePredicates translates predicate alias references from old to
// new via a single-entry AliasMap.
func rebasePredicates(preds []predicates.QueryPredicate, oldAlias, newAlias values.CorrelationIdentifier) []predicates.QueryPredicate {
	if oldAlias == newAlias {
		return preds
	}
	am := values.AliasMap{oldAlias: newAlias}
	result := make([]predicates.QueryPredicate, len(preds))
	for i, p := range preds {
		result[i] = predicates.RebasePredicate(p, am)
	}
	return result
}

var _ ImplementationRule = (*PushDistinctBelowFilterRule)(nil)
