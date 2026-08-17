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

// sortKeysAreOrderable reports whether every sort key has a type an ORDERING
// can be defined on.
//
// Java satisfies a requested ordering only out of PRIMITIVE parts:
// RequestedOrdering.ofPrimitiveParts (RequestedOrdering.java:313-326) is what
// LogicalOperator.generateSort feeds (LogicalOperator.java:552-571), and it
// never expands a record into its leaves the way the GROUPING path does via
// Values.primitiveAccessorsForType (Values.java:99-121). So a RECORD-typed
// ORDER BY key matches no index ordering, no plan survives, and Cascades ends
// at UnableToPlanException — surfacing as 0AF00 "Cascades planner could not
// plan query" (CascadesPlanner.java:407, mapped at ExceptionUtil.java:79-80).
//
// Go reaches a DIFFERENT place for the same query only because Go has an
// in-memory sort fallback that Java's Cascades does not: the fallback accepted
// the record key and the comparator then failed at ROW TIME with a raw
// internal message naming *dynamicpb.Message. Declining to yield here restores
// Java's outcome by Java's own route — the ordering is simply unsatisfiable
// and planning fails — rather than by bolting a type check onto ORDER BY
// parsing, which would diverge the moment the surrounding structure changed.
//
// GROUPING IS DELIBERATELY NOT GATED HERE — and needs no gate: `GROUP BY
// <struct>` WORKS, in both engines, because the grouping path flattens a
// RECORD-typed key into its primitive leaf accessors
// (Values.primitiveAccessorsForType, Values.java:99-121; Go:
// values.PrimitiveAccessorsForType via expandGroupingKeysToPrimitives) —
// its pre-aggregate sort orders leaves, never records, so the comparator
// hazard this rule declines for ORDER BY is unreachable there. Go's
// streaming-aggregation path builds its own sort directly and does not
// pass through this rule.
//
// UNKNOWN is admitted: an untyped key (a bound parameter, an internal
// expression) is not evidence of an unorderable one, and rejecting it here
// would reject shapes that plan and run correctly today.
func sortKeysAreOrderable(sortKeys []expressions.SortKey) bool {
	for _, sk := range sortKeys {
		if sk.Value == nil {
			// DECLINE, not tolerate. A nil Value cannot be sorted BY: the loop
			// below would hand plans.SortKey a nil ValueExpr, and the executor
			// rejects that as a malformed plan. Skipping the key here produced
			// exactly that plan silently, which is the same non-contract the
			// ordering advertiser used to encode a third way. Declining yields
			// no plan — the outcome this rule already gives an unorderable key,
			// and the one the streaming-aggregate twin gives a grouping key
			// with no leaf decomposition.
			return false
		}
		t := sk.Value.Type()
		if values.IsRecord(t) || values.IsArray(t) {
			return false
		}
	}
	return true
}

func (r *ImplementInMemorySortRule) OnMatch(call *ImplementationRuleCall) {
	s := call.Bindings.Get(r.matcher).(*expressions.LogicalSortExpression)
	if s.IsUnsorted() {
		return
	}

	sortKeys := s.GetSortKeys()
	if len(sortKeys) == 0 {
		return
	}
	if !sortKeysAreOrderable(sortKeys) {
		return
	}

	innerRef := s.GetInner().GetRangesOver()
	if innerRef == nil {
		return
	}

	// Top-down: push ordering constraint to inner reference so
	// downstream rules (index scans) can satisfy it. It crosses into the inner
	// reference's own current-row space first, exactly as the dedicated push
	// rule does — see requestedOrderingAtInnerCurrent.
	requestedOrdering, err := requestedOrderingAtInnerCurrent(
		sortExpressionToRequestedOrdering(s), s.GetInner())
	if err != nil {
		call.Fail(err)
		return
	}
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
		field := values.ExplainValue(sk.Value)
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
	logicalEdge, err := s.GetInner().RequireFlowedObjectValue()
	if err != nil {
		call.Fail(err)
		return
	}
	physicalEdge, err := innerQ.RequireFlowedObjectValue()
	if err != nil {
		call.Fail(err)
		return
	}
	primaryKeys, err := rebasePhysicalSortKeys(planKeys, logicalEdge, physicalEdge)
	if err != nil {
		call.Fail(err)
		return
	}
	primarySort, err := plans.NewRecordQueryInMemorySortPlanFromQuantifier(innerQ, primaryKeys)
	if err != nil {
		call.Fail(err)
		return
	}
	call.YieldFinalExpression(primarySort)

	// Also yield InMemorySort alternatives for InJoin/InUnion members
	// and restricted Fetch plans (index scans with bound predicates).
	// These selective plans may have much lower cardinality than the
	// first physical plan, and sorting their small output is cheaper
	// than sorting a full scan. Skip the first physical member: it is the
	// placeholder the group-ranged primary yield above already covers.
	//
	// The member set is physicalMembersForParentEnumeration's — finals, with
	// exploratory only as the no-finals fallback — which is also the set
	// findPhysicalExpr picks the skipped placeholder out of. Enumerating a wider
	// set than the one the skip is computed from is how the placeholder ends up
	// wrapped twice, or a member the cost framework has not admitted as a plan
	// ends up with a parent built over it.
	firstPhys := findPhysicalExpr(innerRef)
	for _, m := range physicalMembersForParentEnumeration(innerRef) {
		if m == firstPhys {
			continue
		}
		// physicalMembersForParentEnumeration only returns physical members, so
		// this holds by construction; the check stays because a rule must not
		// panic if that ever stops being true.
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
		alternativeQ := plans.QuantifierOverPlan(ph.GetRecordQueryPlan())
		alternativeEdge, err := alternativeQ.RequireFlowedObjectValue()
		if err != nil {
			call.Fail(err)
			return
		}
		alternativeKeys, err := rebasePhysicalSortKeys(planKeys, logicalEdge, alternativeEdge)
		if err != nil {
			call.Fail(err)
			return
		}
		alternative, err := plans.NewRecordQueryInMemorySortPlanFromQuantifier(alternativeQ, alternativeKeys)
		if err != nil {
			call.Fail(err)
			return
		}
		call.YieldFinalExpression(alternative)
	}
}

func rebasePhysicalSortKeys(
	keys []plans.SortKey,
	source, target values.QuantifiedObjectValue,
) ([]plans.SortKey, error) {
	result := append([]plans.SortKey(nil), keys...)
	var err error
	for i := range result {
		// A join's logical edge is conventionally named after one of its legs.
		// Alias-only rebasing therefore rewrites that leg's QOV onto the whole
		// joined-row edge even though their exact types disagree, destroying the
		// leg identity before the selected FlatMap can map it to an output slot.
		// The declared-edge bridge requires correlation AND exact type, so a true
		// edge-relative key moves while same-named retained source windows survive
		// for the producer/materializer lineage pass.
		result[i].ValueExpr, err = values.TranslateDeclaredEdgeRoot(
			result[i].ValueExpr, source, target)
		if err != nil {
			return nil, err
		}
	}
	return result, nil
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
	idxPlan := fetchedIndexScan(fetchPlan.GetInner())
	if idxPlan == nil {
		return false
	}
	for _, cr := range idxPlan.GetScanComparisons() {
		if cr != nil && !cr.IsEmpty() {
			return true
		}
	}
	return false
}

// fetchedIndexScan returns the index scan a fetch actually reads entries from,
// looking through the covering wrapper.
//
// The covering plan holds its scan as a FIELD, not a child (RFC-220 C1), so a
// type assertion for the bare scan misses it — and the access path builds
// Fetch(Covering(IndexScan)) for every value-index access, which makes the miss
// TOTAL rather than occasional. The failure is silent in the direction that
// costs plans: no scan found reads as "no comparison ranges", i.e. as an
// unrestricted scan, and the sort-the-selective-output alternative is never
// built for any query. Java reaches the same fields by delegating through the
// covering plan rather than by walking children
// (RecordQueryCoveringIndexPlan.java:224).
func fetchedIndexScan(inner plans.RecordQueryPlan) *plans.RecordQueryIndexPlan {
	switch p := inner.(type) {
	case *plans.RecordQueryIndexPlan:
		return p
	case *plans.RecordQueryCoveringIndexPlan:
		return p.GetIndexPlan()
	default:
		return nil
	}
}

var _ ImplementationRule = (*ImplementInMemorySortRule)(nil)
