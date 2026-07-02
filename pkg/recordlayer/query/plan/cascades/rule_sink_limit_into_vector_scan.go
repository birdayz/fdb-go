package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// SinkLimitIntoVectorScanRule folds a Limit(k) that sits DIRECTLY above a
// distance-ordered VectorIndexScan (RFC-156 Phase B) back into the scan's
// self-limiting top-k mode — restoring the legacy one-shot search(k) fast path
// for the no-residual and partition-only cases, byte-for-byte.
//
// Pattern:
//
//	Limit(k) → VectorIndexScan(ordered)   →   VectorIndexScan(self-limit k)
//
// The rule fires ONLY when the Limit is directly above the ordered vector scan
// with NO intervening row-dropping / order-disturbing operator (i.e. no residual
// Filter). When a residual Filter intervenes the rule does NOT fire, and the
// scan must stream its re-ranked horizon so the Filter+Limit collect the true k
// nearest MATCHING rows — the whole point of the Phase B fix.
//
// This is the decoupled "sink" half of the Cascades match-then-implement split
// (Graefe 1995): the
// match candidate emits ONE canonical ordered-stream form (it never sinks k and
// never introspects residuals); this rule, and only this rule, folds k into the
// scan when it is provably safe to do so.
//
// Modeled on MergeFetchIntoCoveringIndexRule (a physical→physical
// ImplementationRule that yields the inner leaf in place of its wrapper when a
// structural condition holds).
type SinkLimitIntoVectorScanRule struct {
	matcher matching.BindingMatcher
}

func NewSinkLimitIntoVectorScanRule() *SinkLimitIntoVectorScanRule {
	return &SinkLimitIntoVectorScanRule{
		matcher: NewExpressionMatcher[*physicalLimitWrapper]("phys_limit_over_vector"),
	}
}

func (r *SinkLimitIntoVectorScanRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *SinkLimitIntoVectorScanRule) OnMatch(call *ImplementationRuleCall) {
	limitW := matching.Get[*physicalLimitWrapper](call.Bindings, r.matcher)
	if limitW == nil || limitW.plan == nil {
		return
	}
	// A runtime cap (parameterized RFC-156 rank limit `... <= ?`) cannot be
	// folded: K is unknown at plan time, so the scan can't be put into
	// self-limiting top-k(K) mode. It MUST stay an explicit Limit(?) over the
	// ordered stream, which the executor evaluates at run time. (The -1 sentinel
	// also trips the GetLimit() <= 0 guard below, but be explicit.)
	if limitW.plan.GetLimitValue() != nil {
		return
	}
	// An OFFSET cannot be folded into the scan (the scan has no skip), and a
	// non-positive cap is not a real top-k. Leave those as the explicit Limit
	// over the ordered scan.
	if limitW.plan.GetOffset() != 0 || limitW.plan.GetLimit() <= 0 {
		return
	}

	innerRef := limitW.innerQuant.GetRangesOver()
	if innerRef == nil {
		return
	}

	// The inner must be DIRECTLY an ordered-stream vector scan. If a residual
	// Filter (or any other operator) sits between the Limit and the scan, the
	// inner ref's members are filters, not vector scans, and this loop finds
	// nothing — so the rule correctly declines to fire.
	for _, m := range innerRef.AllMembers() {
		vecW, ok := m.(*physicalVectorIndexScanWrapper)
		if !ok || vecW.plan == nil || !vecW.plan.IsOrderedStream() {
			continue
		}
		// A non-literal / non-positive scan-k cannot be folded for ANY member
		// (the scan is the same across the ref's equivalent members), so DECLINE
		// the whole rule — keep the explicit Limit over the ordered scan.
		adjK, okAdj := vectorScanAdjustedLimit(vecW.plan)
		if !okAdj {
			return
		}
		// Fold only when the Limit's cap matches the scan's own adjusted top-k
		// (k for rank<=k, k-1 for rank<k) — i.e. the Limit IS the QUALIFY rank
		// limit, not a tighter independent LIMIT clause. A divergent cap (e.g.
		// `QUALIFY ROW_NUMBER()<=10 LIMIT 3`) must keep the explicit Limit over
		// the ordered scan so the smaller cap is honored, never silently widened
		// to the scan's k.
		if limitW.plan.GetLimit() != adjK {
			continue
		}
		// Yield the self-limiting scan in place of the Limit. The scan's existing
		// k/rankType binding already encodes the QUALIFY rank, so flipping the
		// mode restores the exact legacy search(k).
		call.Yield(&physicalVectorIndexScanWrapper{plan: vecW.plan.WithSelfLimiting()})
		return
	}
}

// vectorScanAdjustedLimit returns the scan's self-limiting top-k (k for
// rank<=k, k-1 for rank<k) when k is a plan-time literal positive int, mirroring
// the executor's getAdjustedLimit. Returns ok=false for a non-literal /
// non-positive k (in which case the SinkLimit fold is declined and the explicit
// Limit stays above the ordered scan).
func vectorScanAdjustedLimit(plan *plans.RecordQueryVectorIndexPlan) (int64, bool) {
	if plan == nil || plan.GetK() == nil {
		return 0, false
	}
	kv, err := plan.GetK().Evaluate(nil)
	if err != nil {
		return 0, false
	}
	var k int64
	switch n := kv.(type) {
	case int:
		k = int64(n)
	case int32:
		k = int64(n)
	case int64:
		k = n
	default:
		return 0, false
	}
	if plan.GetRankType() == predicates.ComparisonDistanceRankLessThan {
		k--
	}
	if k <= 0 {
		return 0, false
	}
	return k, true
}

var _ ImplementationRule = (*SinkLimitIntoVectorScanRule)(nil)
