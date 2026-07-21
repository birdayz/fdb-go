// Go extension — no Java equivalent.
//
// Java's RemoveSortRule (ImplementSortRule in Go) eliminates sorts via
// index ordering or fails. This rule provides an in-memory fallback:
// when no index can satisfy the ORDER BY, materialize and sort.
//
// Registered alongside ImplementSortRule. Both match LogicalSortExpression.
// Cost model ensures index-based elimination is preferred — the in-memory
// sort only wins when it's the sole alternative.
package cascades

import (
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// ImplementInMemorySortRule yields a RecordQueryInMemorySortPlan for any
// LogicalSortExpression whose inner Reference has a physical plan.
// Unlike ImplementSortRule (Java-ported), this does NOT check whether
// the inner ordering already satisfies the sort — it unconditionally
// wraps. The cost model ensures this plan loses to index-based
// elimination when both are available.
type ImplementInMemorySortRule struct {
	matcher matching.BindingMatcher
}

func NewImplementInMemorySortRule() *ImplementInMemorySortRule {
	return &ImplementInMemorySortRule{
		matcher: NewExpressionMatcher[*expressions.LogicalSortExpression]("in_memory_sort"),
	}
}

func (r *ImplementInMemorySortRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *ImplementInMemorySortRule) OnMatch(call *ImplementationRuleCall) {
	s := call.Bindings.Get(r.matcher).(*expressions.LogicalSortExpression)
	if s.IsUnsorted() {
		return
	}

	sortKeys := s.GetSortKeys()
	if len(sortKeys) == 0 {
		return
	}

	innerRef := s.GetInner().GetRangesOver()
	if innerRef == nil {
		return
	}

	// Top-down: push ordering constraint to inner reference so
	// downstream rules (index scans) can satisfy it.
	requestedOrdering := sortExpressionToRequestedOrdering(s)
	call.PushConstraint(innerRef, []*properties.RequestedOrdering{requestedOrdering})

	// Guard: only yield the sort if the inner group has a physical plan to sort.
	// The plan is not baked here — the collapsed sort ranges over innerRef LIVE
	// (below), so its cost and extraction both resolve innerRef's cheapest member.
	if findPhysicalPlan(innerRef) == nil {
		return
	}

	planKeys := make([]plans.SortKey, len(sortKeys))
	for i, sk := range sortKeys {
		// ValueExpr ALWAYS drives the executor — the sort key Value carries its
		// plan-time-baked ordinal (a childless baked output-column key, a
		// CORRELATED/qualified leg reference, or a computed key) and evaluates
		// against the positional row. Field is DISPLAY-ONLY (Explain + the
		// ordering-hint name match). A key that somehow escaped the
		// translator's bake fails loud at evaluation — never a name read.
		field := ""
		if fv, ok := sk.Value.(*values.FieldValue); ok && fv.Child == nil {
			field = strings.ToUpper(values.ColumnNameValue(sk.Value))
		} else {
			field = values.ExplainValue(sk.Value)
		}
		nf := !sk.Reverse // default: ASC→true, DESC→false
		if sk.NullsFirst != nil {
			nf = *sk.NullsFirst
		}
		planKeys[i] = plans.SortKey{Field: field, Desc: sk.Reverse, NullsFirst: nf, ValueExpr: sk.Value}
	}

	// Main arm: the sort ranges over the actual inner group (innerRef) via a LIVE
	// edge, not a baked placeholder. GetInner resolves through planFromQuantifier
	// → innerRef.Winner() — the group's OPTIMIZE-chosen cheapest member — so the
	// cost model (which walks GetChildren) and extraction (which relinks the same
	// edge) sort the SAME member. The wrapper used to bake findPhysicalPlan (the
	// FIRST member, a full-scan placeholder) for COST while extraction rebuilt over
	// findBestPhysicalPlan (the cheapest member): it costed a plan it never emitted.
	// The live edge closes that gap (cost==extraction fix, RFC-184 W2).
	// Ranging over innerRef (not InitialOf(firstMember)) also keeps the good
	// orders-driven join order the group won rather than pinning a re-scan loser
	// (RFC-069).
	innerQ := expressions.ForEachQuantifier(innerRef)
	call.YieldFinalExpression(plans.NewRecordQueryInMemorySortPlanFromQuantifier(innerQ, planKeys))

	// Also yield InMemorySort alternatives for InJoin/InUnion members
	// and restricted Fetch plans (index scans with bound predicates).
	// These selective plans may have much lower cardinality than the
	// first physical plan, and sorting their small output is cheaper
	// than sorting a full scan. Skip the first physical member: it is the
	// placeholder the group-ranged primary yield above already covers.
	firstPhys := findPhysicalExpr(innerRef)
	for _, m := range innerRef.AllMembers() {
		if m == firstPhys {
			continue
		}
		ph, ok := m.(physicalPlanExpression)
		if !ok {
			continue
		}
		wrap := false
		if IsPhysicalInJoin(m) {
			wrap = true
		} else if _, ok := m.(*plans.RecordQueryInUnionPlan); ok {
			// The InUnion is its own cascades expression now (RFC-184 W2).
			wrap = true
		} else if isRestrictedFetch(ph) {
			wrap = true
		}
		if !wrap {
			continue
		}
		// Alt arm: FREEZE the concrete member. This selective member (an InJoin,
		// InUnion, or SARG'd Fetch) is a specific alternative whose small output we
		// want to sort — not the group's overall winner — so the sort snapshots its
		// exact plan. NewRecordQueryInMemorySortPlan builds a bare sort over a
		// frozen (QuantifierOverPlan) edge holding ph's plan; cost and extraction
		// both resolve that one member (RFC-184 W2, no physicalInMemorySortWrapper).
		call.YieldFinalExpression(plans.NewRecordQueryInMemorySortPlan(ph.GetRecordQueryPlan(), planKeys))
	}
}

func (r *ImplementInMemorySortRule) GetRequestedOrderings(
	_ expressions.RelationalExpression,
) []*properties.RequestedOrdering {
	return nil
}

// isRestrictedFetch reports whether a physical plan is a Fetch wrapping
// an IndexScan with at least one non-empty comparison range (a selective
// index lookup, not a full scan).
func isRestrictedFetch(ph physicalPlanExpression) bool {
	fetchPlan, ok := ph.GetRecordQueryPlan().(*plans.RecordQueryFetchFromPartialRecordPlan)
	if !ok {
		return false
	}
	inner := fetchPlan.GetInner()
	if inner == nil {
		return false
	}
	idxPlan, ok := inner.(*plans.RecordQueryIndexPlan)
	if !ok {
		return false
	}
	for _, cr := range idxPlan.GetScanComparisons() {
		if cr != nil && !cr.IsEmpty() {
			return true
		}
	}
	return false
}

var _ ImplementationRule = (*ImplementInMemorySortRule)(nil)
