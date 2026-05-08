package cascades

import (
	"strings"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// PushOrderingThroughGroupByRule pushes a LogicalSort's ordering
// through a GroupByExpression when the sort keys reference group-by
// columns. This enables sort elimination when an index below the
// GROUP BY provides the requested ordering.
//
//	Sort([k1 ASC, k2 DESC], GroupBy(keys=[k1,k2,k3], aggs, X))
//	  → GroupBy(keys=[k1,k2,k3], aggs, Sort([k1 ASC, k2 DESC, k3 ANY], X))
//
// The sort pushed below GroupBy includes:
//   - Each original sort key (preserving direction) — matched against
//     grouping keys by field name (case-insensitive).
//   - Remaining grouping keys not in the sort, appended with ASC
//     (direction doesn't matter for correctness — they only need to
//     be contiguous for streaming aggregation).
//
// If any sort key does NOT match a grouping key, the ordering cannot
// be pushed through and the rule does not fire.
//
// Soundness: GROUP BY is order-agnostic — it partitions rows by key
// equality, not key order. Reordering rows before grouping does not
// change which rows belong to which group. The pushed sort enables
// streaming aggregation (which requires sorted input) while also
// satisfying the outer ORDER BY.
//
// Ports Java's PushRequestedOrderingThroughGroupByRule. Java uses a
// constraint-push model; Go uses a structural rewrite that achieves
// the same effect — the sort moves below GroupBy so that
// SortOverOrderedElimRule can eliminate it if an index provides the
// ordering.
type PushOrderingThroughGroupByRule struct {
	matcher matching.BindingMatcher
}

func NewPushOrderingThroughGroupByRule() *PushOrderingThroughGroupByRule {
	return &PushOrderingThroughGroupByRule{
		matcher: NewExpressionMatcher[*expressions.LogicalSortExpression]("push_ordering_through_groupby"),
	}
}

func (r *PushOrderingThroughGroupByRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *PushOrderingThroughGroupByRule) OnMatch(call *ExpressionRuleCall) {
	s := matching.Get[*expressions.LogicalSortExpression](call.Bindings, r.matcher)
	if s.IsUnsorted() {
		return
	}

	innerExpr := s.GetInner().GetRangesOver().Get()
	gb, ok := innerExpr.(*expressions.GroupByExpression)
	if !ok {
		return
	}

	sortKeys := s.GetSortKeys()
	groupingKeys := gb.GetGroupingKeys()
	if len(groupingKeys) == 0 {
		return
	}

	// Build a set of grouping key field names for quick lookup.
	// Track which grouping keys are consumed by sort keys.
	type groupKeyEntry struct {
		index int
		value values.Value
	}
	groupKeyMap := make(map[string]groupKeyEntry, len(groupingKeys))
	for i, gk := range groupingKeys {
		fv, ok := gk.(*values.FieldValue)
		if !ok {
			// Non-FieldValue grouping key — can't match sort keys.
			return
		}
		groupKeyMap[strings.ToUpper(fv.Field)] = groupKeyEntry{index: i, value: gk}
	}

	// Check that every sort key matches a grouping key.
	consumed := make([]bool, len(groupingKeys))
	newSortKeys := make([]expressions.SortKey, 0, len(groupingKeys))

	for _, sk := range sortKeys {
		fv, ok := sk.Value.(*values.FieldValue)
		if !ok {
			return
		}
		entry, found := groupKeyMap[strings.ToUpper(fv.Field)]
		if !found {
			// Sort key doesn't match any grouping key — can't push.
			return
		}
		if consumed[entry.index] {
			// Duplicate sort key referencing same grouping key.
			return
		}
		consumed[entry.index] = true
		newSortKeys = append(newSortKeys, sk)
	}

	// Append remaining grouping keys (not in the sort) with ASC.
	for i, gk := range groupingKeys {
		if !consumed[i] {
			newSortKeys = append(newSortKeys, expressions.SortKey{
				Value:   gk,
				Reverse: false,
			})
		}
	}

	// Build: GroupBy(keys, aggs, Sort(newSortKeys, gbChild))
	pushedSort := expressions.NewLogicalSortExpression(newSortKeys, gb.GetInner())
	pushedSortQ := expressions.ForEachQuantifier(call.MemoizeExpression(pushedSort))
	newGB := expressions.NewGroupByExpression(gb.GetGroupingKeys(), gb.GetAggregates(), pushedSortQ)
	call.Yield(newGB)
}

var _ ExpressionRule = (*PushOrderingThroughGroupByRule)(nil)
