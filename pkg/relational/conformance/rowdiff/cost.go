package rowdiff

import "fdb.dev/pkg/recordlayer/query/plan/plans"

// Cost/ordering invariants extend the row-soundness harness onto the axis a
// pure row-multiset diff is blind to. The ordinal-binding family of bugs lived
// in how a plan orders and binds its inputs; a diff that only compares result
// rows cannot see a plan that returns the right rows the wrong — or a
// needlessly expensive — way. Each invariant here holds for EVERY correct plan
// of the query: it encodes a property a cost model may never legitimately
// violate, so a violation is a real defect, not a taste difference. Invariants
// that a cost model could reasonably decide either way (index-vs-scan on a
// non-selective predicate) are deliberately excluded — a false finding in a
// nightly safety net is as corrosive as a missed one.

// checkPlanCost returns cost/ordering invariant violations for the plan of q,
// one description per violation (empty = clean). The input is the TYPED
// physical plan tree (never EXPLAIN text), so the checks read structure, not
// rendered strings.
func checkPlanCost(p plans.RecordQueryPlan, q Query) []string {
	var out []string

	// INV — no spurious sort. A bare single-table SELECT with no ordering
	// requirement (no ORDER BY, no SELECT DISTINCT, no aggregate, no
	// correlated subquery) has nothing that could demand a sorted stream, so
	// an in-memory sort in its plan is spurious: a redundant operator, or the
	// symptom of a mis-derived ordering property — the class the
	// ordinal-binding bugs belonged to. Ordered set operations — merge-sort /
	// distinct union and intersections — may legitimately sort index-scan
	// inputs into a common merge order even absent a query ORDER BY (an OR
	// predicate lowers to exactly this), so a plan carrying a set-op is
	// conservatively excluded rather than flagged.
	if singleTableNoOrderingReq(q) && !planHasSetOp(p) && planHasInMemorySort(p) {
		out = append(out,
			"spurious in-memory sort: single-table query states no ordering requirement and the plan has no set-operation to merge")
	}
	return out
}

// singleTableNoOrderingReq reports whether q is a bare single-table SELECT that
// imposes no ordering on its result: no join / union / derived source, no
// correlated subquery, no aggregate, no DISTINCT, and no ORDER BY. For such a
// query, no operator in a correct plan needs a sorted input.
func singleTableNoOrderingReq(q Query) bool {
	return q.Join == nil && q.ThreeWay == nil && q.Union == nil &&
		q.Derived == nil && q.Exists == nil && q.ScalarSub == nil &&
		q.Agg == nil && !q.Distinct && len(q.OrderBy) == 0
}

// planHasInMemorySort reports whether the plan tree contains an in-memory sort.
func planHasInMemorySort(p plans.RecordQueryPlan) bool {
	return planAny(p, func(n plans.RecordQueryPlan) bool {
		_, ok := n.(*plans.RecordQueryInMemorySortPlan)
		return ok
	})
}

// planHasSetOp is a conservative family check for any intersection or union.
// Only keyed merge variants require sorted inputs, but keeping every set-op out
// of this coarse invariant avoids misclassifying a mixed-family plan.
func planHasSetOp(p plans.RecordQueryPlan) bool {
	return planAny(p, func(n plans.RecordQueryPlan) bool {
		switch n.(type) {
		case *plans.RecordQueryIntersectionPlan,
			*plans.RecordQueryMultiIntersectionOnValuesPlan,
			*plans.RecordQueryUnionPlan,
			*plans.RecordQueryUnorderedUnionPlan,
			*plans.RecordQueryMergeSortUnionPlan,
			*plans.RecordQueryInUnionPlan:
			return true
		}
		return false
	})
}

// planAny reports whether any node in the plan tree satisfies pred.
func planAny(p plans.RecordQueryPlan, pred func(plans.RecordQueryPlan) bool) bool {
	if p == nil {
		return false
	}
	if pred(p) {
		return true
	}
	for _, c := range p.GetChildren() {
		if planAny(c, pred) {
			return true
		}
	}
	return false
}
