package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
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
	// retires the Go-only validateNoIndexOnlyResidual late net + compensationSafeForYield
	// index-only branch.
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
		orderings = []*RequestedOrdering{PreserveOrdering()}
	}

	seen := make(map[expressions.RelationalExpression]bool)
	for _, ordering := range orderings {
		winner := getWinnerForOrdering(innerRef, ordering, call.CostModel())
		if winner == nil {
			continue
		}
		if seen[winner] {
			continue
		}
		seen[winner] = true
		ph, ok := winner.(physicalPlanExpression)
		if !ok {
			continue
		}
		innerAlias := f.GetInner().GetAlias()
		filterPlan := plans.NewRecordQueryPredicatesFilterPlanWithAlias(ph.GetRecordQueryPlan(), f.GetPredicates(), innerAlias)
		innerQ := expressions.ForEachQuantifier(call.MemoizeExpression(winner))
		call.Yield(NewPhysicalPredicatesFilterWrapper(filterPlan, innerQ))
	}
}

var _ ExpressionRule = (*ImplementFilterRule)(nil)
