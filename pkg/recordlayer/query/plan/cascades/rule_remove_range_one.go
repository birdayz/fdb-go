package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
)

// RemoveRangeOneRule eliminates a redundant LIMIT 1 when the inner
// expression is guaranteed to produce at most one row.
//
// Pattern:
//
//	LogicalLimit(limit=1, offset=0)
//	  inner → X   [where X has cardinality ≤ 1]
//
// Rewrite: X
//
// The inner expression is considered at-most-one-row when its
// estimated cardinality is ≤ 1.0 (via properties.EstimateCardinality).
// This covers unique-index equality scans, scalar subquery results,
// LogicalValuesExpression (single-row constant source), and the
// zero-row empty-scan sentinel.
type RemoveRangeOneRule struct {
	matcher matching.BindingMatcher
}

func NewRemoveRangeOneRule() *RemoveRangeOneRule {
	return &RemoveRangeOneRule{
		matcher: NewExpressionMatcher[*expressions.LogicalLimitExpression]("logical_limit"),
	}
}

func (r *RemoveRangeOneRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *RemoveRangeOneRule) OnMatch(call *ExpressionRuleCall) {
	lim := matching.Get[*expressions.LogicalLimitExpression](call.Bindings, r.matcher)
	// A runtime cap (parameterized RFC-156 rank limit) is not a literal LIMIT 1.
	if lim.GetLimitValue() != nil {
		return
	}
	// Only match LIMIT 1 OFFSET 0.
	if lim.GetLimit() != 1 || lim.GetOffset() != 0 {
		return
	}

	innerExpr := lim.GetInner().GetRangesOver().Get()
	if innerExpr == nil {
		return
	}

	if !isAtMostOneRow(innerExpr) {
		return
	}

	call.Yield(innerExpr)
}

// isAtMostOneRow reports whether the expression is guaranteed to
// produce at most one row. Uses the cardinality estimate from the
// properties cost model as the primary signal. Falls back to
// structural type checks for expression types whose cost-model
// cardinality may not yet reflect the true single-row nature
// (e.g. LogicalValuesExpression).
func isAtMostOneRow(e expressions.RelationalExpression) bool {
	// Cardinality-based check: covers physical wrappers and any
	// expression whose cost model is calibrated to report ≤ 1 row.
	if properties.EstimateCardinality(e) <= 1.0 {
		return true
	}

	// Structural fallbacks for expression types whose cost model
	// does not yet report single-row cardinality.
	switch e.(type) {
	case *expressions.LogicalValuesExpression:
		// VALUES produces exactly one row of constants.
		return true
	}

	return false
}

var _ ExpressionRule = (*RemoveRangeOneRule)(nil)
