package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
)

// PushFilterThroughProjectionRule pushes a LogicalFilter under a
// LogicalProjection.
//
//	Filter(P, Projection([col1, col2], X))  →  Projection([col1, col2], Filter(P, X))
//
// Soundness: LogicalProjection in the seed doesn't reshape rows —
// its GetResultValue is the inner's flowed object value (the
// projection list is a side channel describing what columns the
// projection EXPOSES; the rows themselves carry all columns of X).
// FieldValue references inside P resolve against the same row shape
// either side of the rewrite. Output row sets are equal:
// "rows of X passing P, with the projection list applied" on both
// sides.
//
// SQL note: this matches the semantic ordering — WHERE is logically
// evaluated BEFORE the SELECT projection, so the rewrite reflects
// the SQL spec.
//
// Optimization argument: filtering BEFORE projection saves the
// projection from materialising rows that would be filtered away.
// Negligible for the seed (projection is a side channel) but
// significant once the executor materialises projected columns.
//
// Termination: yields a Projection wrapping a Quantifier over a
// fresh Reference holding the new Filter. The fresh Reference is
// caught by Reference.Insert's SemanticEquals fallback.
type PushFilterThroughProjectionRule struct {
	matcher matching.BindingMatcher
}

// NewPushFilterThroughProjectionRule constructs the rule.
func NewPushFilterThroughProjectionRule() *PushFilterThroughProjectionRule {
	return &PushFilterThroughProjectionRule{
		matcher: NewExpressionMatcher[*expressions.LogicalFilterExpression]("logical_filter"),
	}
}

// Matcher returns the pattern.
func (r *PushFilterThroughProjectionRule) Matcher() matching.BindingMatcher { return r.matcher }

// OnMatch fires when the inner Quantifier ranges over a
// LogicalProjectionExpression.
func (r *PushFilterThroughProjectionRule) OnMatch(call *ExpressionRuleCall) {
	f := matching.Get[*expressions.LogicalFilterExpression](call.Bindings, r.matcher)
	innerExpr := f.GetInner().GetRangesOver().Get()
	p, ok := innerExpr.(*expressions.LogicalProjectionExpression)
	if !ok {
		return
	}
	pushed := expressions.NewLogicalFilterExpression(f.GetPredicates(), p.GetInner())
	pushedQ := expressions.ForEachQuantifier(call.MemoizeExpression(pushed))
	call.Yield(expressions.NewLogicalProjectionExpression(p.GetProjectedValues(), pushedQ))
}

var _ ExpressionRule = (*PushFilterThroughProjectionRule)(nil)
