package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// ImplementFilterRule implements a logical LogicalFilterExpression
// as a physical RecordQueryPredicatesFilterPlan, provided the inner
// Quantifier's Reference contains at least one physical-plan member.
//
//	Filter([P], inner-with-physical-member)
//	  →  PredicatesFilterPlan([P], inner-physical, innerAlias)
//
// Matches Java's ImplementFilterRule which creates
// RecordQueryPredicatesFilterPlan with a physical quantifier morphed
// from the logical inner quantifier, preserving the alias. The alias
// is critical for correlated predicates: the executor binds the
// current row under innerAlias so QOV-based predicates can resolve.
//
// The "with-physical-member" guard ensures the rule fires only AFTER
// the inner has been implemented (i.e. PrimaryScanRule or another
// implement rule has yielded a physical wrapper into the inner's
// Reference). This avoids producing partial physical plans that
// reference still-logical inner trees.
type ImplementFilterRule struct {
	matcher matching.BindingMatcher
}

// NewImplementFilterRule constructs the rule.
func NewImplementFilterRule() *ImplementFilterRule {
	return &ImplementFilterRule{
		matcher: NewExpressionMatcher[*expressions.LogicalFilterExpression]("logical_filter"),
	}
}

// Matcher returns the pattern.
func (r *ImplementFilterRule) Matcher() matching.BindingMatcher { return r.matcher }

// OnMatch fires on every LogicalFilterExpression. Walks the inner
// Reference for any physical-plan member; if found, yields a
// FilterPlan wrapper over a fresh Reference holding that physical
// inner.
func (r *ImplementFilterRule) OnMatch(call *ExpressionRuleCall) {
	f := matching.Get[*expressions.LogicalFilterExpression](call.Bindings, r.matcher)

	// Java's ImplementFilterRule binds `all(anyCompensatablePredicate())` where the
	// extractor is `!isIndexOnly()` (ImplementFilterRule.java:62 +
	// QueryPredicateMatchers.java:66-68): the rule fires only when EVERY predicate is
	// compensatable. A predicate carrying an index-only value (a vector DistanceRank /
	// UnmatchedAggregateValue marker that has no executable form outside the index
	// access) cannot be evaluated by a RecordQueryPredicatesFilterPlan at runtime, so
	// the rule must not synthesize one. When the index legitimately serves the
	// index-only predicate, the data-access match consumes it into the scan (and, with
	// the partial-match re-trigger in TransformExprTask, is consumed without relying on
	// this rule's incidental yield); when nothing can serve it the rule's non-firing
	// leaves the query correctly unplannable. This is the structural Java gate that
	// retires the compensationSafeForYield index-only branch (B4). Note:
	// validateNoIndexOnlyResidual is RETAINED as the catch-all backstop for Go-only
	// physical-filter builders (ImplementSimpleSelectRule, NLJ, ImplementIndexScanRule)
	// that this gate does not cover — do not remove it until every such builder is gated.
	for _, pred := range f.GetPredicates() {
		if predicateContainsUncompensatableValues(pred) {
			return
		}
	}

	innerRef := f.GetInner().GetRangesOver()
	if innerRef == nil {
		return
	}

	orderings := call.GetRequestedOrderings()
	if len(orderings) == 0 {
		orderings = []*properties.RequestedOrdering{properties.PreserveOrdering()}
	}

	seen := make(map[expressions.RelationalExpression]bool)
	// ENUMERATE, don't single-pick. Java fires an implementation rule once per
	// (parent, child-member) pair — a rule binds every member of a child
	// reference — so both Filter(Fetch(Covering)) and Filter(Index) reach the
	// parent group and COST decides between them. Go used to build only the
	// per-ordering winner, so a child member that is not the local winner was
	// never lifted into a parent alternative at all: not out-priced, never
	// constructed.
	//
	// This is not ranking at the rule site (which physical_wrapper.go's
	// findPhysicalExpr comment rightly forbids, because a rule-time cost pick is
	// an ordering-blind second optimizer). It is the opposite: the rule stops
	// choosing and hands every valid child to the memo.
	//
	// The per-ordering winners below are still yielded — they are what guarantees
	// an ORDERING-satisfying alternative exists — and the enumeration adds the
	// members no ordering asked for.
	//
	// Each enumerated parent ranges over a reference RESTRICTED to its one child
	// member (Java's memoizeMemberPlansFromOther, ImplementFilterRule.java:89),
	// NOT over the interned group. Interning returns the group that already
	// CONTAINS the member — here, innerRef itself — so every iteration would
	// build the structurally identical Filter(innerRef) and the N alternatives
	// would collapse into one on insert. The restriction is what makes them
	// distinct; without it this loop yields N times and enumerates nothing.
	for _, m := range physicalMembersForParentEnumeration(innerRef) {
		if seen[m] {
			continue
		}
		seen[m] = true
		innerAlias := f.GetInner().GetAlias()
		innerQ := expressions.NamedForEachQuantifier(innerAlias, call.MemoizeMemberPlansFromOther(
			innerRef, []expressions.RelationalExpression{m}))
		filterPlan, err := plans.NewRecordQueryPredicatesFilterPlanWithAliasFromQuantifier(
			innerQ, f.GetPredicates(), innerAlias)
		if err != nil {
			call.Fail(err)
			return
		}
		call.Yield(filterPlan)
	}
	for _, ordering := range orderings {
		// satisfied deliberately DISCARDED (RFC-186 §2C): this wrapper is an
		// orderingDelegator — its ordering claim is re-derived through
		// OrderingSourceRef at lookup time and pinOrderedSpine declines
		// unsatisfied spines, so an unordered fallback yield here can never
		// be CLAIMED as ordered; it is the member the in-memory-sort
		// enforcer wraps (declining instead would empty the group — no plan).
		winner, _ := getWinnerForOrdering(innerRef, ordering, call.CostModel())
		if winner == nil {
			continue
		}
		if seen[winner] {
			continue
		}
		seen[winner] = true
		if _, ok := winner.(physicalPlanExpression); !ok {
			continue
		}
		innerAlias := f.GetInner().GetAlias()
		// The filter carries the LIVE inner edge over the winner's shared group
		// (RFC-184 W2) — exactly the edge the wrapper's innerQuant presented. A
		// plain filter's inner has no correlated SARG snapshot to preserve (unlike
		// the FoD/NLJ paths), so it ranges over the live group: this keeps
		// push_filter_through_fetch's re-explored pushed member reachable from a
		// parent that captures this leg. A frozen snapshot strands the pre-push
		// filter once the merged group canonicalizes to the pushed one.
		innerQ := expressions.ForEachQuantifier(call.MemoizeExpression(winner))
		filterPlan, err := plans.NewRecordQueryPredicatesFilterPlanWithAliasFromQuantifier(innerQ, f.GetPredicates(), innerAlias)
		if err != nil {
			call.Fail(err)
			return
		}
		call.Yield(filterPlan)
	}
}

var _ ExpressionRule = (*ImplementFilterRule)(nil)
