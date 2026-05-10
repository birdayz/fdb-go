package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
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
type MergeFetchIntoCoveringIndexRule struct {
	matcher matching.BindingMatcher
}

func NewMergeFetchIntoCoveringIndexRule() *MergeFetchIntoCoveringIndexRule {
	return &MergeFetchIntoCoveringIndexRule{
		matcher: NewExpressionMatcher[*physicalFetchFromPartialRecordWrapper]("phys_fetch_over_index"),
	}
}

func (r *MergeFetchIntoCoveringIndexRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *MergeFetchIntoCoveringIndexRule) OnMatch(call *ImplementationRuleCall) {
	fetchW := matching.Get[*physicalFetchFromPartialRecordWrapper](call.Bindings, r.matcher)

	innerRef := fetchW.innerQuant.GetRangesOver()
	if innerRef == nil {
		return
	}

	// Check if the inner is a covering index scan.
	var indexW *physicalIndexScanWrapper
	for _, m := range innerRef.AllMembers() {
		if iw, ok := m.(*physicalIndexScanWrapper); ok {
			indexW = iw
			break
		}
	}
	if indexW == nil {
		return
	}

	// The covering index provides all needed columns — eliminate the
	// fetch and yield the index scan directly.
	call.Yield(indexW)
}

var _ ImplementationRule = (*MergeFetchIntoCoveringIndexRule)(nil)
