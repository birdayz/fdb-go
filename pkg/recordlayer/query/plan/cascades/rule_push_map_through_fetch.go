package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
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
		matcher: NewExpressionMatcher[*physicalMapWrapper]("phys_map_over_fetch"),
	}
}

func (r *PushMapThroughFetchRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *PushMapThroughFetchRule) OnMatch(call *ImplementationRuleCall) {
	mapW := matching.Get[*physicalMapWrapper](call.Bindings, r.matcher)

	innerRef := mapW.innerQuant.GetRangesOver()
	if innerRef == nil {
		return
	}

	// Find the fetch wrapper in the map's inner.
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
	resultValue := mapW.plan.GetResultValue()

	// Try to push the result value through the fetch. Uses recursive
	// decomposition so composite values (RecordConstructorValue, etc.)
	// are translated leaf-by-leaf matching Java's mapMaybe approach.
	oldAlias := mapW.innerQuant.GetAlias()
	newInnerAlias := values.UniqueCorrelationIdentifier()

	pushedResultValue := tryTranslateValue(fetchPlan, oldAlias, newInnerAlias, resultValue)
	if pushedResultValue == nil {
		return
	}

	// Get the fetch's inner (covering index scan).
	fetchInnerRef := fetchW.innerQuant.GetRangesOver()
	if fetchInnerRef == nil {
		return
	}
	fetchInnerExpr := findPhysicalExpr(fetchInnerRef)
	if fetchInnerExpr == nil {
		return
	}

	// Mark the inner index scan as covering since the fetch is eliminated.
	if idxW, ok := fetchInnerExpr.(*physicalIndexScanWrapper); ok && !idxW.covering {
		coveredPlan := idxW.plan.WithCovering(idxW.columnNames)
		fetchInnerExpr = &physicalIndexScanWrapper{
			plan:        coveredPlan,
			columnNames: idxW.columnNames,
			unique:      idxW.unique,
			covering:    true,
		}
	}

	// Build: Map(translatedResultValue, fetchInner)
	// The fetch is eliminated entirely.
	pushedMapPlan := plans.NewRecordQueryMapPlan(nil, pushedResultValue)
	newInnerQ := expressions.ForEachQuantifier(
		call.MemoizeFinalExpressionsFromOther(fetchInnerRef, []expressions.RelationalExpression{fetchInnerExpr}),
	)
	pushedMapWrapper := NewPhysicalMapWrapper(pushedMapPlan, newInnerQ)

	call.Yield(pushedMapWrapper)
}

var _ ImplementationRule = (*PushMapThroughFetchRule)(nil)
