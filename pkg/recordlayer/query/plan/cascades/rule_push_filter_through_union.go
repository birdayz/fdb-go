package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
)

// PushFilterThroughUnionRule distributes a LogicalFilter over a
// LogicalUnion's children — the classic distribution rewrite.
//
//	Filter(P, Union(A, B, ...))  →  Union(Filter(P, A), Filter(P, B), ...)
//
// Soundness: row-set equivalence — filtering the union is the same
// as filtering each operand and unioning the results. Holds for
// UNION ALL semantics (which our LogicalUnionExpression represents);
// also remains sound under UNION DISTINCT (Distinct over the result)
// because the dedup commutes with filter and union.
//
// Optimization argument: distributing the filter into each operand
// gives downstream physical-plan rules (B5 Batch A) a chance to push
// the predicate INTO each operand's scan / index — a much bigger win
// than filtering the unioned stream once.
//
// Note: this rule produces a structurally LARGER tree (N filters
// instead of 1). The benefit comes from follow-on rules; without
// downstream pushdown, the rewrite is a wash. Plausible cost-model
// regression; the seed implements it directly because (a) the
// SemanticEquals fallback in Reference.Insert means re-firing on
// the same input is dedup'd, and (b) the larger seed memo is what
// the cost-driven extraction (B4 follow-on) needs to compare.
type PushFilterThroughUnionRule struct {
	matcher matching.BindingMatcher
}

// NewPushFilterThroughUnionRule constructs the rule.
func NewPushFilterThroughUnionRule() *PushFilterThroughUnionRule {
	return &PushFilterThroughUnionRule{
		matcher: NewExpressionMatcher[*expressions.LogicalFilterExpression]("logical_filter"),
	}
}

// Matcher returns the pattern.
func (r *PushFilterThroughUnionRule) Matcher() matching.BindingMatcher { return r.matcher }

// OnMatch fires when the inner Quantifier ranges over a
// LogicalUnionExpression with at least one child.
func (r *PushFilterThroughUnionRule) OnMatch(call *ExpressionRuleCall) {
	f := matching.Get[*expressions.LogicalFilterExpression](call.Bindings, r.matcher)
	innerExpr := f.GetInner().GetRangesOver().Get()
	u, ok := innerExpr.(*expressions.LogicalUnionExpression)
	if !ok {
		return
	}
	children := u.GetQuantifiers()
	if len(children) == 0 {
		return
	}
	pushed := make([]expressions.Quantifier, 0, len(children))
	for _, child := range children {
		// Each operand: Filter(P, child-as-Quantifier). Reuse the
		// child Quantifier so the Filter shares the same Reference
		// pointer as the child input — pointer-identity dedup hits
		// on second fire of the rule.
		fc := expressions.NewLogicalFilterExpression(f.GetPredicates(), child)
		pushed = append(pushed, expressions.ForEachQuantifier(call.MemoizeExpression(fc)))
	}
	call.Yield(expressions.NewLogicalUnionExpression(pushed))
}

var _ ExpressionRule = (*PushFilterThroughUnionRule)(nil)
