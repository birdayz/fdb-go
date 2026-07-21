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
		matcher: NewExpressionMatcher[*plans.RecordQueryInJoinPlan]("phys_injoin_over_fetch"),
	}
}

func (r *PushInJoinThroughFetchRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *PushInJoinThroughFetchRule) OnMatch(call *ImplementationRuleCall) {
	// The InJoin is its own cascades expression now (RFC-184 W2).
	inJoinPlan := matching.Get[*plans.RecordQueryInJoinPlan](call.Bindings, r.matcher)

	// Java excludes InComparandJoinPlan: comparand values depend on the
	// outer record and cannot be safely pushed past a fetch boundary.
	if inJoinPlan.GetSourceKind() == plans.InSourceComparand {
		return
	}

	innerRef := inJoinPlan.GetInnerQuantifier().GetRangesOver()
	if innerRef == nil {
		return
	}

	// Find the fetch in the InJoin's inner.
	var fetchW *plans.RecordQueryFetchFromPartialRecordPlan
	for _, m := range innerRef.AllMembers() {
		if fw, ok := m.(*plans.RecordQueryFetchFromPartialRecordPlan); ok {
			fetchW = fw
			break
		}
	}
	if fetchW == nil {
		return
	}

	fetchPlan := fetchW

	// Get the fetch's inner (covering index scan).
	fetchInnerRef := fetchW.GetInnerQuantifier().GetRangesOver()
	if fetchInnerRef == nil {
		return
	}
	fetchInnerExpr := findPhysicalExpr(fetchInnerRef)
	if fetchInnerExpr == nil {
		return
	}
	// Guard only: the pushed InJoin ranges over the live fetch-inner edge, so it
	// no longer needs the baked snapshot — but the inner must still be a real
	// physical plan for the push to be valid.
	if bakedInnerPlan(fetchInnerExpr) == nil {
		return
	}

	// Build: InJoin(fetchInner) as its own cascades expression carrying the live
	// fetch-inner edge (RFC-184 W2); no plan snapshot. The extracted inner
	// resolves via ref.Winner() over the singleton fetch-inner group.
	innerQ := expressions.NewPhysicalQuantifier(
		call.MemoizeFinalExpressionsFromOther(fetchInnerRef, []expressions.RelationalExpression{fetchInnerExpr}),
	)
	pushedInJoinPlan := plans.NewRecordQueryInJoinPlanFromQuantifier(
		innerQ,
		inJoinPlan.GetBindingName(),
		inJoinPlan.IsSorted(),
		inJoinPlan.IsReverse(),
	)
	if inValues := inJoinPlan.GetInValues(); inValues != nil {
		pushedInJoinPlan.SetInValues(inValues)
	}
	pushedInJoinPlan.SetSourceKind(inJoinPlan.GetSourceKind())

	// Memoize the pushed InJoin.
	pushedInJoinRef := call.MemoizeFinalExpression(pushedInJoinPlan)

	// Build: Fetch(InJoin(fetchInner)) as its own cascades expression carrying
	// the live pushedInJoinRef edge (RFC-184 W2).
	newFetchQ := expressions.ForEachQuantifier(pushedInJoinRef)
	newFetchPlan := plans.NewRecordQueryFetchFromPartialRecordPlanFromQuantifier(
		newFetchQ,
		fetchPlan.GetTranslateValueFunction(),
		fetchPlan.GetResultType(),
		fetchPlan.GetFetchIndexRecords(),
	)

	call.Yield(newFetchPlan)
}

var _ ImplementationRule = (*PushInJoinThroughFetchRule)(nil)
