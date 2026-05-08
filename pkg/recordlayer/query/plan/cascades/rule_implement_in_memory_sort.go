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

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
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
		matcher: &inMemorySortMatcher{},
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
	call.PushConstraint(innerRef, []*RequestedOrdering{requestedOrdering})

	innerPlan := findPhysicalPlan(innerRef)
	if innerPlan == nil {
		return
	}

	planKeys := make([]plans.SortKey, len(sortKeys))
	for i, sk := range sortKeys {
		field := ""
		var valExpr values.Value
		if fv, ok := sk.Value.(*values.FieldValue); ok {
			field = strings.ToUpper(fv.Field)
		} else {
			field = values.ExplainValue(sk.Value)
			valExpr = sk.Value
		}
		nf := !sk.Reverse // default: ASC→true, DESC→false
		if sk.NullsFirst != nil {
			nf = *sk.NullsFirst
		}
		planKeys[i] = plans.SortKey{Field: field, Desc: sk.Reverse, NullsFirst: nf, ValueExpr: valExpr}
	}

	sortPlan := plans.NewRecordQueryInMemorySortPlan(innerPlan, planKeys)

	innerExpr := findPhysicalExpr(innerRef)
	if innerExpr == nil {
		return
	}
	innerQ := expressions.ForEachQuantifier(expressions.InitialOf(innerExpr))
	call.YieldFinalExpression(newPhysicalInMemorySortWrapper(sortPlan, innerQ))
}

func (r *ImplementInMemorySortRule) GetRequestedOrderings(
	_ expressions.RelationalExpression,
) []*RequestedOrdering {
	return nil
}

type inMemorySortMatcher struct{}

func (m *inMemorySortMatcher) RootType() string { return "LogicalSortExpression" }

func (m *inMemorySortMatcher) BindMatches(outer *matching.PlannerBindings, in any) []*matching.PlannerBindings {
	if _, ok := in.(*expressions.LogicalSortExpression); !ok {
		return nil
	}
	return []*matching.PlannerBindings{outer.Bind(m, in)}
}

var _ ImplementationRule = (*ImplementInMemorySortRule)(nil)
