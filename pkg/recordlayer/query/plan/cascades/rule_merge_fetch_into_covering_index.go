package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// MergeFetchIntoCoveringIndexRule eliminates a
// FetchFromPartialRecordPlan when its inner is a covering index scan.
// If the index provides all needed columns (the fetch is redundant),
// the rule yields just the inner index scan plan directly.
//
// Pattern:
//
//	Fetch(CoveringIndexScan)  →  IndexScan
//
// Mirrors Java's `MergeFetchIntoCoveringIndexRule` one for one, including what
// it yields: the inner INDEX plan, which fetches its own records by primary
// key. The rule fires when the fetch wraps a covering scan directly (no
// intervening filter or distinct).
//
// It was documented as unreachable, because coveringness was a flag that the
// access path applied only by STRIPPING the fetch. Now the access path emits
// Fetch(Covering(IndexScan)) on every value-index match (RFC-220), which is
// exactly this rule's pattern, so it is live.
type MergeFetchIntoCoveringIndexRule struct {
	matcher matching.BindingMatcher
}

func NewMergeFetchIntoCoveringIndexRule() *MergeFetchIntoCoveringIndexRule {
	return &MergeFetchIntoCoveringIndexRule{
		matcher: NewExpressionMatcher[*plans.RecordQueryFetchFromPartialRecordPlan]("phys_fetch_over_index"),
	}
}

func (r *MergeFetchIntoCoveringIndexRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *MergeFetchIntoCoveringIndexRule) OnMatch(call *ImplementationRuleCall) {
	fetchW := matching.Get[*plans.RecordQueryFetchFromPartialRecordPlan](call.Bindings, r.matcher)

	innerRef := fetchW.GetInnerQuantifier().GetRangesOver()
	if innerRef == nil {
		return
	}

	// Java: fetchFromPartialRecordPlan(coveringIndexPlan().where(indexPlanOf(innerPlanMatcher)))
	// → call.yieldPlan(innerPlan). The inner INDEX plan is yielded, not the
	// covering plan: a bare index plan resolves each entry to its base record
	// by primary key, so Fetch(Covering(Index)) and Index are the same rows
	// from one node instead of two. Go's executor agrees — executeIndexScan
	// resolves records via indexFetchCursor.
	var indexPlan *plans.RecordQueryIndexPlan
	for _, m := range innerRef.AllMembers() {
		if cov, ok := m.(*plans.RecordQueryCoveringIndexPlan); ok {
			indexPlan = cov.GetIndexPlan()
			break
		}
	}
	if indexPlan == nil {
		return
	}

	call.Yield(indexPlan)
}

var _ ImplementationRule = (*MergeFetchIntoCoveringIndexRule)(nil)
