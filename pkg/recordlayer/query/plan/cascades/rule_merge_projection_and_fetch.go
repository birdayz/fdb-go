package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
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
		matcher: NewExpressionMatcher[*physicalProjectionWrapper]("phys_projection_merge_fetch"),
	}
}

func (r *MergeProjectionAndFetchRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *MergeProjectionAndFetchRule) OnMatch(call *ImplementationRuleCall) {
	projW := matching.Get[*physicalProjectionWrapper](call.Bindings, r.matcher)

	innerRef := projW.innerQuant.GetRangesOver()
	if innerRef == nil {
		return
	}

	// Find the fetch wrapper in the projection's inner.
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
	projectedValues := projW.plan.GetProjections()

	oldInnerAlias := projW.innerQuant.GetAlias()
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
	fetchInnerRef := fetchW.innerQuant.GetRangesOver()
	if fetchInnerRef == nil {
		return
	}
	fetchInnerExpr := findPhysicalExpr(fetchInnerRef)
	if fetchInnerExpr == nil {
		return
	}

	if idxW, ok := fetchInnerExpr.(*physicalIndexScanWrapper); ok && !idxW.covering {
		coveredPlan := idxW.plan.WithCovering(idxW.columnNames)
		coveringIdxW := &physicalIndexScanWrapper{
			plan:        coveredPlan,
			columnNames: idxW.columnNames,
			unique:      idxW.unique,
			covering:    true,
		}
		coveringRef := expressions.InitialOf(coveringIdxW)
		innerQ := expressions.ForEachQuantifier(coveringRef)
		wrapPlan := plans.NewRecordQueryProjectionPlanWithAliases(
			projectedValues, projW.plan.GetAliases(), coveredPlan)
		call.Yield(NewPhysicalProjectionWrapper(wrapPlan, innerQ))
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
	childPhys, ok := fetchInnerExpr.(physicalPlanExpression)
	if !ok {
		return
	}
	projPlan := plans.NewRecordQueryProjectionPlanWithAliases(
		projectedValues, projW.plan.GetAliases(), childPhys.GetRecordQueryPlan())
	call.Yield(NewPhysicalProjectionWrapper(projPlan, expressions.ForEachQuantifier(fetchInnerRef)))
}

var _ ImplementationRule = (*MergeProjectionAndFetchRule)(nil)
