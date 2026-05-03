package cascades

import (
	"strings"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// PushFilterThroughGroupByRule pushes a LogicalFilter below a
// GroupByExpression when ALL predicates reference only the grouping
// keys.
//
//	Filter(P, GroupBy(keys, aggs, X))  →  GroupBy(keys, aggs, Filter(P, X))
//
// Soundness: if P references only grouping-key columns, filtering
// before aggregation produces the same groups — rows eliminated by P
// wouldn't contribute to any group that survives P.
//
// Only fires when EVERY predicate can be pushed. Partial pushdown
// (splitting pushed vs. residual predicates) is a future extension.
//
// Java equivalent: PushPredicateThroughGroupByRule — pushes comparison
// predicates that reference grouping keys.
type PushFilterThroughGroupByRule struct {
	matcher matching.BindingMatcher
}

func NewPushFilterThroughGroupByRule() *PushFilterThroughGroupByRule {
	return &PushFilterThroughGroupByRule{
		matcher: NewExpressionMatcher[*expressions.LogicalFilterExpression]("logical_filter"),
	}
}

func (r *PushFilterThroughGroupByRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *PushFilterThroughGroupByRule) OnMatch(call *ExpressionRuleCall) {
	f := matching.Get[*expressions.LogicalFilterExpression](call.Bindings, r.matcher)
	innerExpr := f.GetInner().GetRangesOver().Get()
	gb, ok := innerExpr.(*expressions.GroupByExpression)
	if !ok {
		return
	}

	groupKeySet := buildGroupKeySet(gb.GetGroupingKeys())
	if len(groupKeySet) == 0 && len(gb.GetGroupingKeys()) > 0 {
		return
	}

	for _, p := range f.GetPredicates() {
		if !predicateReferencesOnlyKeys(p, groupKeySet) {
			return
		}
	}

	pushed := expressions.NewLogicalFilterExpression(f.GetPredicates(), gb.GetInner())
	pushedQ := expressions.ForEachQuantifier(call.MemoizeExpression(pushed))
	call.Yield(expressions.NewGroupByExpression(gb.GetGroupingKeys(), gb.GetAggregates(), pushedQ))
}

func buildGroupKeySet(keys []values.Value) map[string]struct{} {
	m := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		fv, ok := k.(*values.FieldValue)
		if !ok {
			return nil
		}
		m[strings.ToUpper(fv.Field)] = struct{}{}
	}
	return m
}

func predicateReferencesOnlyKeys(p predicates.QueryPredicate, keySet map[string]struct{}) bool {
	cp, ok := p.(*predicates.ComparisonPredicate)
	if !ok {
		if _, isConst := p.(*predicates.ConstantPredicate); isConst {
			return true
		}
		return false
	}
	fv, ok := cp.Operand.(*values.FieldValue)
	if !ok {
		return false
	}
	_, inKeys := keySet[strings.ToUpper(fv.Field)]
	return inKeys
}

var _ ExpressionRule = (*PushFilterThroughGroupByRule)(nil)
