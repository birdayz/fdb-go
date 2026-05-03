package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
)

// LimitMergeRule consolidates nested LIMIT expressions into one.
//
// Pattern:
//
//	LogicalLimit(limitOuter, offsetOuter)
//	  inner → LogicalLimit(limitInner, offsetInner)
//	    inner → X
//
// Rewrite:
//
//	LogicalLimit(effectiveLimit, effectiveOffset)
//	  inner → X
//
// Semantics: LIMIT a OFFSET b over LIMIT c OFFSET d
//
//	The inner produces at most c rows starting at d.
//	The outer then skips b of those and takes a.
//	Combined offset = d + b (skip d from source, then b more).
//	Combined limit = min(a, c - b) — can't take more than inner
//	  produces minus what outer skips of the inner result.
//	If c - b <= 0, the combined limit is 0 (no rows).
type LimitMergeRule struct {
	matcher matching.BindingMatcher
}

func NewLimitMergeRule() *LimitMergeRule {
	return &LimitMergeRule{
		matcher: NewExpressionMatcher[*expressions.LogicalLimitExpression]("logical_limit"),
	}
}

func (r *LimitMergeRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *LimitMergeRule) OnMatch(call *ExpressionRuleCall) {
	outer := matching.Get[*expressions.LogicalLimitExpression](call.Bindings, r.matcher)
	innerExpr := outer.GetInner().GetRangesOver().Get()
	inner, ok := innerExpr.(*expressions.LogicalLimitExpression)
	if !ok {
		return
	}

	oLimit := outer.GetLimit()
	oOffset := outer.GetOffset()
	iLimit := inner.GetLimit()
	iOffset := inner.GetOffset()

	combinedOffset := iOffset + oOffset

	var combinedLimit int64
	if iLimit < 0 {
		combinedLimit = oLimit
	} else {
		available := iLimit - oOffset
		if available <= 0 {
			combinedLimit = 0
		} else if oLimit < 0 || available < oLimit {
			combinedLimit = available
		} else {
			combinedLimit = oLimit
		}
	}

	merged := expressions.NewLogicalLimitExpression(combinedLimit, combinedOffset, inner.GetInner())
	call.Yield(merged)
}

var _ ExpressionRule = (*LimitMergeRule)(nil)
