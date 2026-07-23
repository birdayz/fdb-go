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
		// Since RFC-184 W2 the memo holds the bare *plans.RecordQueryDistinctPlan
		// (no physicalDistinctWrapper).
		matcher: NewExpressionMatcher[*plans.RecordQueryDistinctPlan]("phys_distinct_over_fetch"),
	}
}

func (r *PushDistinctThroughFetchRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *PushDistinctThroughFetchRule) OnMatch(call *ImplementationRuleCall) {
	distinctW := matching.Get[*plans.RecordQueryDistinctPlan](call.Bindings, r.matcher)

	innerRef := distinctW.GetInnerQuantifier().GetRangesOver()
	if innerRef == nil {
		return
	}

	// Find the fetch in distinct's inner.
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

	// Get the fetch's inner (the covering index scan).
	fetchInnerRef := fetchW.GetInnerQuantifier().GetRangesOver()
	if fetchInnerRef == nil {
		return
	}
	fetchInnerExpr := findPhysicalExpr(fetchInnerRef)
	if fetchInnerExpr == nil {
		return
	}
	fetchInnerPlan := bakedInnerPlan(fetchInnerExpr)
	if fetchInnerPlan == nil {
		return
	}

	// Build: Distinct(fetchInner) as its own cascades expression (RFC-184 W2, no
	// physicalDistinctWrapper). Recompute the streaming mode against the NEW inner
	// (the fetch's inner): a fetch preserves ordering, so the distinct below it is
	// still streaming-eligible when that inner is ordered by the dedup key — a
	// constructor reset would otherwise drop a resume-clean streaming distinct to
	// the memory-heavy hash-set as a competing alternative (TODO C5: cross-page
	// correctness fixed 2026-07-20; streaming preferred for O(1) memory).
	streaming := distinctStreamingEligible(fetchInnerExpr, fetchInnerPlan)
	// The distinct's edge ranges over the BAKED concrete inner frozen in a
	// detached single-member final reference — the memo-canonical structure
	// push_filter_through_fetch case-2 uses, NOT the live fetchInnerExpr memo edge
	// (whose children may still be holes). The alias is carried from the
	// disentangled member ref so the flowed value stays stable.
	baseQ := expressions.ForEachQuantifier(
		call.MemoizeFinalExpressionsFromOther(fetchInnerRef, []expressions.RelationalExpression{fetchInnerExpr}),
	)
	newDistinctInnerQ := expressions.NamedForEachQuantifier(baseQ.GetAlias(),
		call.MemoizeFinalExpression(fetchInnerPlan))
	newDistinctPlan := plans.NewRecordQueryDistinctPlanFromQuantifier(newDistinctInnerQ, streaming)

	// Memoize the distinct.
	distinctRef := call.MemoizeFinalExpression(newDistinctPlan)

	// Build: Fetch(Distinct(fetchInner)) as its own cascades expression carrying
	// the live distinctRef edge (RFC-184 W2).
	newFetchQ := expressions.ForEachQuantifier(distinctRef)
	newFetchPlan := plans.NewRecordQueryFetchFromPartialRecordPlanFromQuantifier(
		newFetchQ,
		fetchW.GetTranslateValueFunction(),
		fetchW.GetResultType(),
		fetchW.GetFetchIndexRecords(),
	)

	call.Yield(newFetchPlan)
}

var _ ImplementationRule = (*PushDistinctThroughFetchRule)(nil)
