package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// PushDistinctThroughFetchRule pushes a physical
// RecordQueryUnorderedPrimaryKeyDistinctPlan through a
// RecordQueryFetchFromPartialRecordPlan to reduce the number of
// records before the (expensive) fetch.
//
// Pattern:
//
//	Distinct(Fetch(inner))  →  Fetch(Distinct(inner))
//
// The distinct can operate on partial records (index entries carry the
// PK needed for deduplication), so pushing it below the fetch avoids
// fetching duplicates.
//
// Mirrors Java's `PushDistinctThroughFetchRule`.
type PushDistinctThroughFetchRule struct {
	matcher matching.BindingMatcher
}

func NewPushDistinctThroughFetchRule() *PushDistinctThroughFetchRule {
	return &PushDistinctThroughFetchRule{
		matcher: NewExpressionMatcher[*physicalDistinctWrapper]("phys_distinct_over_fetch"),
	}
}

func (r *PushDistinctThroughFetchRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *PushDistinctThroughFetchRule) OnMatch(call *ImplementationRuleCall) {
	distinctW := matching.Get[*physicalDistinctWrapper](call.Bindings, r.matcher)

	innerRef := distinctW.innerQuant.GetRangesOver()
	if innerRef == nil {
		return
	}

	// Find the fetch wrapper in distinct's inner.
	var fetchW *physicalFetchFromPartialRecordWrapper
	for _, m := range innerRef.AllMembers() {
		if fw, ok := m.(*physicalFetchFromPartialRecordWrapper); ok {
			fetchW = fw
			break
		}
	}
	if fetchW == nil {
		return
	}

	// Get the fetch's inner (the covering index scan).
	fetchInnerRef := fetchW.innerQuant.GetRangesOver()
	if fetchInnerRef == nil {
		return
	}
	fetchInnerExpr := findPhysicalExpr(fetchInnerRef)
	if fetchInnerExpr == nil {
		return
	}

	// Build: Distinct(fetchInner)
	newDistinctPlan := plans.NewRecordQueryDistinctPlan(nil)
	newDistinctQ := expressions.ForEachQuantifier(
		call.MemoizeFinalExpressionsFromOther(fetchInnerRef, []expressions.RelationalExpression{fetchInnerExpr}),
	)
	newDistinctWrapper := NewPhysicalDistinctWrapper(newDistinctPlan, newDistinctQ)

	// Memoize the distinct.
	distinctRef := call.MemoizeFinalExpression(newDistinctWrapper)

	// Build: Fetch(Distinct(fetchInner))
	newFetchPlan := plans.NewRecordQueryFetchFromPartialRecordPlan(
		nil,
		fetchW.plan.GetTranslateValueFunction(),
		fetchW.plan.GetResultType(),
		fetchW.plan.GetFetchIndexRecords(),
	)
	newFetchQ := expressions.ForEachQuantifier(distinctRef)
	newFetchWrapper := NewPhysicalFetchFromPartialRecordWrapper(newFetchPlan, newFetchQ)

	call.Yield(newFetchWrapper)
}

var _ ImplementationRule = (*PushDistinctThroughFetchRule)(nil)
