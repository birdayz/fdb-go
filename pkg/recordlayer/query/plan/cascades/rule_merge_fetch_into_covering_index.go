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
// In Go, covering index scans are physicalIndexScanWrappers whose
// TranslateValueFunction can translate all required values. The rule
// fires when the fetch wraps an index scan directly (no intermediate
// filter or distinct).
//
// Mirrors Java's `MergeFetchIntoCoveringIndexRule`.
//
// Note: currently unreachable in the production pipeline because
// wrapScanPlanWithCoverage strips the Fetch at construction time for
// covering indexes (returning a bare physicalIndexScanWrapper with
// covering=true). This rule exists for completeness and would fire if
// a Fetch(covering-index) structure were produced by a different path
// (e.g., manual plan construction or future rule rewrites).
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

	// Check if the inner is a covering index scan marked as covering.
	// In Java, this rule matches FetchPlan(CoveringIndexPlan(IndexPlan))
	// where CoveringIndexPlan is an explicit wrapper indicating the
	// index provides all needed columns. In Go, we check the index
	// scan wrapper's `covering` flag (set by the data access pipeline
	// when the index is known to cover all referenced fields).
	// The index scan is its own cascades expression now (RFC-184 W2) — carrying its
	// covering flag on the plan, no physicalIndexScanWrapper.
	var indexPlan *plans.RecordQueryIndexPlan
	for _, m := range innerRef.AllMembers() {
		if ip, ok := m.(*plans.RecordQueryIndexPlan); ok {
			if ip.IsCovering() {
				indexPlan = ip
				break
			}
		}
	}
	if indexPlan == nil {
		return
	}

	call.Yield(indexPlan)
}

var _ ImplementationRule = (*MergeFetchIntoCoveringIndexRule)(nil)
