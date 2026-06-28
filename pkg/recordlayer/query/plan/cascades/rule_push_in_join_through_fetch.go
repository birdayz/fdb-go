package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// PushInJoinThroughFetchRule pushes a RecordQueryInJoinPlan through a
// FetchFromPartialRecordPlan. The InJoin runs on the partial/index
// records (cheaper), and the Fetch is lifted to the top.
//
// Before:
//
//	InJoin(Fetch(inner))
//
// After:
//
//	Fetch(InJoin(inner))
//
// Go collapses Java's 3 InJoin subclasses (InValuesJoin,
// InParameterJoin, InComparandJoin) into a single
// RecordQueryInJoinPlan, so only one rule instance is needed
// (Java instantiates this rule twice).
//
// Mirrors Java's PushInJoinThroughFetchRule.
type PushInJoinThroughFetchRule struct {
	matcher matching.BindingMatcher
}

func NewPushInJoinThroughFetchRule() *PushInJoinThroughFetchRule {
	return &PushInJoinThroughFetchRule{
		matcher: NewExpressionMatcher[*physicalInJoinWrapper]("phys_injoin_over_fetch"),
	}
}

func (r *PushInJoinThroughFetchRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *PushInJoinThroughFetchRule) OnMatch(call *ImplementationRuleCall) {
	inJoinW := matching.Get[*physicalInJoinWrapper](call.Bindings, r.matcher)

	// Java excludes InComparandJoinPlan: comparand values depend on the
	// outer record and cannot be safely pushed past a fetch boundary.
	if inJoinW.plan != nil && inJoinW.plan.GetSourceKind() == plans.InSourceComparand {
		return
	}

	innerRef := inJoinW.innerQuant.GetRangesOver()
	if innerRef == nil {
		return
	}

	// Find the fetch wrapper in the InJoin's inner.
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

	fetchPlan := fetchW.plan

	// Get the fetch's inner (covering index scan).
	fetchInnerRef := fetchW.innerQuant.GetRangesOver()
	if fetchInnerRef == nil {
		return
	}
	fetchInnerExpr := findPhysicalExpr(fetchInnerRef)
	if fetchInnerExpr == nil {
		return
	}

	// Build: InJoin(fetchInner)
	// Create a new InJoinPlan with the fetch's inner as its child.
	inJoinPlan := inJoinW.plan
	pushedInJoinPlan := plans.NewRecordQueryInJoinPlan(
		nil, // inner is tracked by wrapper, not plan
		inJoinPlan.GetBindingName(),
		inJoinPlan.IsSorted(),
		inJoinPlan.IsReverse(),
	)
	if inValues := inJoinPlan.GetInValues(); inValues != nil {
		pushedInJoinPlan.SetInValues(inValues)
	}
	pushedInJoinPlan.SetSourceKind(inJoinPlan.GetSourceKind())

	innerQ := expressions.NewPhysicalQuantifier(
		call.MemoizeFinalExpressionsFromOther(fetchInnerRef, []expressions.RelationalExpression{fetchInnerExpr}),
	)
	pushedInJoinWrapper := NewPhysicalInJoinWrapper(pushedInJoinPlan, innerQ)

	// Memoize the pushed InJoin.
	pushedInJoinRef := call.MemoizeFinalExpression(pushedInJoinWrapper)

	// Build: Fetch(InJoin(fetchInner))
	newFetchPlan := plans.NewRecordQueryFetchFromPartialRecordPlan(
		nil,
		fetchPlan.GetTranslateValueFunction(),
		fetchPlan.GetResultType(),
		fetchPlan.GetFetchIndexRecords(),
	)
	newFetchQ := expressions.ForEachQuantifier(pushedInJoinRef)
	newFetchWrapper := NewPhysicalFetchFromPartialRecordWrapper(newFetchPlan, newFetchQ)

	call.Yield(newFetchWrapper)
}

var _ ImplementationRule = (*PushInJoinThroughFetchRule)(nil)
