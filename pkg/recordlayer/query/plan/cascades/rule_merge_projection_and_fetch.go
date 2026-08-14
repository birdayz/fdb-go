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
	fetchInnerRef := fetchW.GetInnerQuantifier().GetRangesOver()
	if fetchInnerRef == nil {
		return
	}
	newInnerQ := expressions.ForEachQuantifier(fetchInnerRef)
	newInnerAlias := newInnerQ.GetAlias()

	// Check if ALL projected values can be pushed through the fetch.
	allPushable := true
	pushedValues := make([]values.Value, len(projectedValues))
	for i, v := range projectedValues {
		pushed, ok := fetchPlan.PushValue(v, oldInnerAlias, newInnerAlias)
		if !ok || pushed == nil {
			allPushable = false
			break
		}
		pushedValues[i] = pushed
	}

	if !allPushable {
		return
	}

	// All fields in the projection are already available underneath
	// the fetch. We don't need the projection nor the fetch — yield
	// the fetch's inner child directly, marked as covering.
	fetchInnerExpr := findPhysicalExpr(fetchInnerRef)
	if fetchInnerExpr == nil {
		return
	}

	// The rule now does what Java's MergeProjectionAndFetchRule.onMatch does:
	// remove the fetch and use its CHILD. No shape check, no type assertion, no
	// coveringness stamped — the child was built as Covering(IndexScan) at the
	// access path and is covering however deep it sits beneath operators pushed
	// below the fetch.
	//
	// ONE DELIBERATE, PERMANENT DIVERGENCE (RFC-220 §4.1): Go RETAINS the
	// projection where Java drops it (`yieldPlan(fetchPlan.getChild())`). Java
	// can drop it because its covering plan's getResultValue() returns the
	// un-projected BASE record type — it carries a standing TODO admitting so —
	// and Java simply tolerates the wider output. Go's covering plan reports the
	// same full partial-record shape, but Go's callers do not tolerate a wider
	// row, so dropping the projection would leak the full record and the wrong
	// output schema. Retaining it is strictly more correct than Java, not a
	// fallback pending a port; there is no result-value rewrite in Java to port.
	// Recorded in DIVERGENCES.md.
	//
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
	projection, err := plans.NewRecordQueryProjectionPlanFromQuantifierWithOutputSchema(
		pushedValues, projW.GetAliases(), projW.GetAliasMinted(), projW.GetOutputNames(), newInnerQ)
	if err != nil {
		call.Fail(err)
		return
	}
	projection, err = projection.WithAliasSources(projW.GetAliasSources())
	if err != nil {
		call.Fail(err)
		return
	}
	call.Yield(projection)
}

var _ ImplementationRule = (*MergeProjectionAndFetchRule)(nil)
