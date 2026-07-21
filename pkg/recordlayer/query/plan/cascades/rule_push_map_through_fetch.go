package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// PushMapThroughFetchRule pushes a map expression through a
// FetchFromPartialRecordPlan when all values referenced by the map's
// result value can be translated to the partial-record (index) domain.
// This eliminates the fetch entirely — the map runs directly on the
// covering index scan.
//
// Pattern:
//
//	Map(resultValue, Fetch(inner))  →  Map(translatedResultValue, inner)
//
// The fetch is completely eliminated because the map reshapes the output
// in a way that detaches downstream data flow from the full record.
//
// Mirrors Java's `PushMapThroughFetchRule`.
type PushMapThroughFetchRule struct {
	matcher matching.BindingMatcher
}

func NewPushMapThroughFetchRule() *PushMapThroughFetchRule {
	return &PushMapThroughFetchRule{
		matcher: NewExpressionMatcher[*plans.RecordQueryMapPlan]("phys_map_over_fetch"),
	}
}

func (r *PushMapThroughFetchRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *PushMapThroughFetchRule) OnMatch(call *ImplementationRuleCall) {
	mapW := matching.Get[*plans.RecordQueryMapPlan](call.Bindings, r.matcher)

	innerRef := mapW.GetInnerQuantifier().GetRangesOver()
	if innerRef == nil {
		return
	}

	// Find the fetch in the map's inner.
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
	resultValue := mapW.GetResultValue()

	// Try to push the result value through the fetch. Uses recursive
	// decomposition so composite values (RecordConstructorValue, etc.)
	// are translated leaf-by-leaf matching Java's mapMaybe approach.
	oldAlias := mapW.GetInnerQuantifier().GetAlias()
	newInnerAlias := values.UniqueCorrelationIdentifier()

	pushedResultValue := tryTranslateValue(fetchPlan, oldAlias, newInnerAlias, resultValue)
	if pushedResultValue == nil {
		return
	}

	// Get the fetch's inner (covering index scan).
	fetchInnerRef := fetchW.GetInnerQuantifier().GetRangesOver()
	if fetchInnerRef == nil {
		return
	}
	fetchInnerExpr := findPhysicalExpr(fetchInnerRef)
	if fetchInnerExpr == nil {
		return
	}

	// Mark the inner index scan as covering since the fetch is eliminated. The
	// index scan is its own cascades expression (RFC-184 W2); WithCovering
	// preserves the metadata already on the plan (struct copy).
	if idxPlan, ok := fetchInnerExpr.(*plans.RecordQueryIndexPlan); ok && !idxPlan.IsCovering() {
		fetchInnerExpr = idxPlan.WithCovering(idxPlan.GetColumnNames())
	}

	// Build: Map(translatedResultValue, fetchInner)
	// The fetch is eliminated entirely.
	//
	// The child plan is read AFTER the covering rewrite above: the rewritten
	// wrapper carries a different (covering) index plan, and baking the
	// pre-rewrite plan would put a non-covering scan under a map whose fetch
	// has been eliminated.
	fetchInnerPlan := bakedInnerPlan(fetchInnerExpr)
	if fetchInnerPlan == nil {
		return
	}
	newInnerQ := expressions.ForEachQuantifier(
		call.MemoizeFinalExpressionsFromOther(fetchInnerRef, []expressions.RelationalExpression{fetchInnerExpr}),
	)
	// The pushed projection (Map) is its own cascades expression carrying the
	// live newInnerQ edge (RFC-184 W2).
	pushedMapPlan := plans.NewRecordQueryMapPlanFromQuantifier(newInnerQ, pushedResultValue)

	call.Yield(pushedMapPlan)
}

var _ ImplementationRule = (*PushMapThroughFetchRule)(nil)
