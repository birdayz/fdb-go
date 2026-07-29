package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// MergeProjectionAndFetchRule removes both a LogicalProjectionExpression
// and a FetchFromPartialRecordPlan when all projected values are
// available in the partial record (index entry) before the fetch.
//
// If every projected value can be pushed through the fetch (translated
// from the full-record domain to the partial-record domain), then
// neither the projection nor the fetch is needed: the fetch's inner
// (covering index scan) already provides all necessary data.
//
// Before:
//
//	Projection(Fetch(inner))
//
// After (when all values pushable):
//
//	inner
//
// Mirrors Java's MergeProjectionAndFetchRule.
type MergeProjectionAndFetchRule struct {
	matcher matching.BindingMatcher
}

func NewMergeProjectionAndFetchRule() *MergeProjectionAndFetchRule {
	return &MergeProjectionAndFetchRule{
		matcher: NewExpressionMatcher[*plans.RecordQueryProjectionPlan]("phys_projection_merge_fetch"),
	}
}

func (r *MergeProjectionAndFetchRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *MergeProjectionAndFetchRule) OnMatch(call *ImplementationRuleCall) {
	projW := matching.Get[*plans.RecordQueryProjectionPlan](call.Bindings, r.matcher)

	innerRef := projW.GetInnerQuantifier().GetRangesOver()
	if innerRef == nil {
		return
	}

	// Find the fetch in the projection's inner.
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
	projectedValues := projW.GetProjections()

	oldInnerAlias := projW.GetInnerQuantifier().GetAlias()
	newInnerAlias := values.UniqueCorrelationIdentifier()

	// Check if ALL projected values can be pushed through the fetch.
	allPushable := true
	for _, v := range projectedValues {
		if _, ok := fetchPlan.PushValue(v, oldInnerAlias, newInnerAlias); !ok {
			allPushable = false
			break
		}
	}

	if !allPushable {
		return
	}

	// All fields in the projection are already available underneath
	// the fetch. We don't need the projection nor the fetch — yield
	// the fetch's inner child directly, marked as covering.
	fetchInnerRef := fetchW.GetInnerQuantifier().GetRangesOver()
	if fetchInnerRef == nil {
		return
	}
	fetchInnerExpr := findPhysicalExpr(fetchInnerRef)
	if fetchInnerExpr == nil {
		return
	}

	if idxPlan, ok := fetchInnerExpr.(*plans.RecordQueryIndexPlan); ok && !idxPlan.IsCovering() {
		// The covering index scan is its own cascades expression (RFC-184 W2);
		// WithCovering preserves the metadata already on the plan (struct copy).
		coveredPlan := idxPlan.WithCovering(idxPlan.GetColumnNames())
		innerQ := expressions.ForEachQuantifier(expressions.InitialOf(coveredPlan))
		// The projection is its own cascades expression carrying the live innerQ
		// edge (RFC-184 W2).
		call.Yield(plans.NewRecordQueryProjectionPlanFromQuantifierWithProvenance(
			projectedValues, projW.GetAliases(), projW.GetAliasMinted(), innerQ))
		return
	}

	// Fallback: the fetch's child is not a directly-coverable index scan
	// (e.g. it is an InJoin whose own inner is already a covering scan,
	// produced by PushInJoinThroughFetchRule). The fetch is removable
	// because all projected values are available in the partial record,
	// but — unlike Java, where pushValue rewrites the child's result value
	// to the projected columns — Go's covering plans carry the FULL
	// partial-record result value. So the projection MUST be retained to
	// select the queried columns; dropping it (Java's
	// `yieldPlan(fetchPlan.getChild())`) leaks the full record and the
	// wrong output schema. The covering-index branch above retains the
	// projection for exactly this reason.
	// fetchInnerExpr comes from findPhysicalExpr above, which only returns
	// physicalPlanExpression members, so !ok is unreachable; the guard is
	// defensive (avoids a panic if that invariant ever changes).
	if _, ok := fetchInnerExpr.(physicalPlanExpression); !ok {
		return
	}
	// The projection is its own cascades expression carrying the live
	// fetch-inner edge (RFC-184 W2): ranging over fetchInnerRef directly makes
	// the projection sit over the fetch's index-scan inner (the merge that
	// strips the fetch), and DAG-aware extraction resolves that shared inner
	// group to its winner instead of the retired snapshot.
	call.Yield(plans.NewRecordQueryProjectionPlanFromQuantifierWithProvenance(
		projectedValues, projW.GetAliases(), projW.GetAliasMinted(), expressions.ForEachQuantifier(fetchInnerRef)))
}

var _ ImplementationRule = (*MergeProjectionAndFetchRule)(nil)
