package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
)

// PushOrderingThroughTempTableInsertRule pushes a LogicalSort's
// ordering through a TempTableInsertExpression. TempTableInsert
// passes through rows into a temp table — sorting below produces the
// same final order as sorting above.
//
//	Sort([k1 ASC], TempTableInsert(alias, X))
//	  → TempTableInsert(alias, Sort([k1 ASC], X))
//
// No key translation is needed: sort keys pass through unchanged.
//
// Ports Java's PushRequestedOrderingThroughInsertTempTableRule.
type PushOrderingThroughTempTableInsertRule struct {
	matcher matching.BindingMatcher
}

func NewPushOrderingThroughTempTableInsertRule() *PushOrderingThroughTempTableInsertRule {
	return &PushOrderingThroughTempTableInsertRule{
		matcher: NewExpressionMatcher[*expressions.LogicalSortExpression]("push_ordering_through_temp_table_insert"),
	}
}

func (r *PushOrderingThroughTempTableInsertRule) Matcher() matching.BindingMatcher {
	return r.matcher
}

func (r *PushOrderingThroughTempTableInsertRule) OnMatch(call *ExpressionRuleCall) {
	s := matching.Get[*expressions.LogicalSortExpression](call.Bindings, r.matcher)
	if s.IsUnsorted() {
		return
	}

	innerExpr := s.GetInner().GetRangesOver().Get()
	tti, ok := innerExpr.(*expressions.TempTableInsertExpression)
	if !ok {
		return
	}

	// Build: TempTableInsert(alias, Sort(keys, ttiChild))
	pushedSort := expressions.NewLogicalSortExpression(s.GetSortKeys(), tti.GetInner())
	pushedSortQ := expressions.ForEachQuantifier(call.MemoizeExpression(pushedSort))
	newTTI := expressions.NewTempTableInsertExpression(pushedSortQ, tti.GetTempTableAlias(), tti.IsOwning())
	call.Yield(newTTI)
}

var _ ExpressionRule = (*PushOrderingThroughTempTableInsertRule)(nil)
