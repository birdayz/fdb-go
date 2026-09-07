package cascades

import (
	"math"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
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
	// A runtime cap (parameterized RFC-156 rank limit) can't be min-merged with a
	// static cap at plan time — the merged limit reads the -1 sentinel and would
	// silently drop the runtime bound. Leave the two LIMITs stacked.
	if outer.GetLimitValue() != nil || inner.GetLimitValue() != nil {
		return
	}

	oLimit := outer.GetLimit()
	oOffset := outer.GetOffset()
	iLimit := inner.GetLimit()
	iOffset := inner.GetOffset()

	combinedOffset, ok := checkedLimitSum(iOffset, oOffset)
	if !ok {
		// A semantic skip cannot saturate: retain the two exact windows when
		// their combined offset cannot be represented by a single LIMIT.
		return
	}

	var combinedLimit int64
	if iLimit < 0 {
		combinedLimit = oLimit
	} else {
		// Both operands are nonnegative (offsets are validated at construction),
		// so subtraction cannot underflow int64 even when no rows remain.
		available := iLimit - oOffset
		if available <= 0 {
			combinedLimit = 0
		} else if oLimit < 0 || available < oLimit {
			combinedLimit = available
		} else {
			combinedLimit = oLimit
		}
	}

	merged, err := expressions.NewLogicalLimitExpression(combinedLimit, combinedOffset, inner.GetInner())
	if err != nil {
		call.Fail(err)
		return
	}
	call.Yield(merged)
}

// checkedLimitSum adds finite, nonnegative row counts. Negative LIMIT values
// are no-cap sentinels, not summands; an overflowing sum cannot be represented
// by a static limit or offset.
func checkedLimitSum(a, b int64) (int64, bool) {
	if a < 0 || b < 0 || a > math.MaxInt64-b {
		return 0, false
	}
	return a + b, true
}

var _ ExpressionRule = (*LimitMergeRule)(nil)
