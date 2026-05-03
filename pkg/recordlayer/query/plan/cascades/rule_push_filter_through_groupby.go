package cascades

import (
	"strings"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// PushFilterThroughGroupByRule pushes predicates from a LogicalFilter
// below a GroupByExpression when those predicates reference only the
// grouping keys. Supports partial pushdown: pushable predicates move
// below GroupBy; residual predicates stay as a filter above.
//
//	Filter([P1, P2], GroupBy(keys, aggs, X))
//	  → Filter([P2], GroupBy(keys, aggs, Filter([P1], X)))  [P1 on keys, P2 not]
//	  → GroupBy(keys, aggs, Filter([P1, P2], X))            [all on keys]
//
// Soundness: if a predicate references only grouping-key columns,
// filtering before aggregation produces the same groups — rows
// eliminated by the predicate wouldn't contribute to any group that
// survives it.
//
// Java equivalent: PushPredicateThroughGroupByRule.
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

	var pushable, residual []predicates.QueryPredicate
	for _, p := range f.GetPredicates() {
		if predicateReferencesOnlyKeys(p, groupKeySet) {
			pushable = append(pushable, p)
		} else {
			residual = append(residual, p)
		}
	}
	if len(pushable) == 0 {
		return
	}

	pushed := expressions.NewLogicalFilterExpression(pushable, gb.GetInner())
	pushedQ := expressions.ForEachQuantifier(call.MemoizeExpression(pushed))
	newGB := expressions.NewGroupByExpression(gb.GetGroupingKeys(), gb.GetAggregates(), pushedQ)

	if len(residual) == 0 {
		call.Yield(newGB)
	} else {
		gbQ := expressions.ForEachQuantifier(call.MemoizeExpression(newGB))
		call.Yield(expressions.NewLogicalFilterExpression(residual, gbQ))
	}
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
