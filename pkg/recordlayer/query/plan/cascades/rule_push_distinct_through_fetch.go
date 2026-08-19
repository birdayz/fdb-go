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
//	PrimaryKeyDistinct(Fetch(inner))  →  Fetch(PrimaryKeyDistinct(inner))
//
// The dedup key must be the PRIMARY KEY for this to be sound, and that is why
// the rule matches the primary-key node and not the full-row one. Below the
// fetch the rows are PARTIAL records. A fetch is 1:1 from record to row but not
// injective over partial rows: two partial rows that differ can map to the SAME
// record, which is exactly what happens when the inner is a union of covering
// scans over DIFFERENT indexes — one contributes (a, pk), the other (b, a, pk).
// A full-row dedup then collapses nothing and the record is fetched once per
// index that produced it, while the same dedup ABOVE the fetch (over whole
// records) would have collapsed them. The primary key is carried identically by
// every partial row for a record, so the primary-key dedup is unaffected by
// where it sits.
//
// Mirrors Java's `PushDistinctThroughFetchRule`, whose root matcher is
// `unorderedPrimaryKeyDistinctPlan(fetchFromPartialRecordPlan(anyPlan()))`.
type PushDistinctThroughFetchRule struct {
	matcher matching.BindingMatcher
}

func NewPushDistinctThroughFetchRule() *PushDistinctThroughFetchRule {
	return &PushDistinctThroughFetchRule{
		matcher: NewExpressionMatcher[*plans.RecordQueryUnorderedPrimaryKeyDistinctPlan](
			"phys_pk_distinct_over_fetch"),
	}
}

func (r *PushDistinctThroughFetchRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *PushDistinctThroughFetchRule) OnMatch(call *ImplementationRuleCall) {
	distinctW := matching.Get[*plans.RecordQueryUnorderedPrimaryKeyDistinctPlan](call.Bindings, r.matcher)

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

	// Build: PrimaryKeyDistinct(fetchInner) as its own cascades expression
	// (RFC-184 W2, no physicalDistinctWrapper). There is no streaming mode to
	// re-derive here: the primary-key distinct has a single hash executor, and
	// the ordering-critical flag that the full-row distinct carries — and that a
	// push has to recompute against the new inner — does not exist on this node.
	//
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
	newDistinctPlan, err := plans.NewRecordQueryUnorderedPrimaryKeyDistinctPlanFromQuantifier(newDistinctInnerQ)
	if err != nil {
		call.Fail(err)
		return
	}

	// Memoize the distinct.
	distinctRef := call.MemoizeFinalExpression(newDistinctPlan)

	// Build: Fetch(Distinct(fetchInner)) as its own cascades expression carrying
	// the live distinctRef edge (RFC-184 W2).
	newFetchQ := expressions.ForEachQuantifier(distinctRef)
	newFetchPlan, err := plans.NewRecordQueryFetchFromPartialRecordPlanFromQuantifier(
		newFetchQ,
		fetchW.GetTranslateValueFunction(),
		fetchW.GetResultType(),
		fetchW.GetFetchIndexRecords(),
	)
	if err != nil {
		call.Fail(err)
		return
	}

	call.Yield(newFetchPlan)
}

var _ ImplementationRule = (*PushDistinctThroughFetchRule)(nil)
