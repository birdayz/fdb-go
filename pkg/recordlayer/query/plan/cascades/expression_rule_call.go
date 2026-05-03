package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
)

// ExpressionRuleCall is the rule-invocation context used by
// RelationalExpression-shaped rules. Counterpart to the existing
// RuleCall (which targets QueryPredicate / Value rules) — split per
// type so each rule shape gets a strongly-typed Yield.
//
// Ports the seed surface of Java's
// `com.apple.foundationdb.record.query.plan.cascades.CascadesRuleCall`.
// Java's class is 632 lines covering exploratory / final / plan /
// partial-match yields, planner-phase plumbing, and traversal-state
// hooks. The seed exposes:
//
//   - Bindings: the pattern matcher's results, keyed by matcher
//     identity (already provided by `matching.PlannerBindings`).
//   - Reference: the memo group whose member fired the rule. Yields
//     append to this Reference; the dedup happens via Reference.Insert.
//   - Context: the PlanContext (planner config + match candidates).
//   - Memo: the Memo for cross-Reference memoization (nil when running
//     outside the Planner, e.g. in standalone tests).
//   - Yield(expr): insert a new equivalent expression into the
//     Reference.
//   - MemoizeExpression(expr): find-or-create a Reference for a
//     sub-expression via the Memo. Falls back to InitialOf when no
//     Memo is present.
//   - Yielded(): the list of expressions yielded so far. Tests + the
//     planner's traversal driver consume this.
//
// The four flavoured yields (exploratory / final / plan / unknown)
// collapse to one Yield until the planner phases / Memo flavour
// distinctions actually matter (B5 / B6 follow-on).
type ExpressionRuleCall struct {
	Bindings    *matching.PlannerBindings
	Reference   *expressions.Reference
	Context     PlanContext
	memo        *Memo
	yieldedExps []expressions.RelationalExpression
}

// NewExpressionRuleCall builds a rule-call against a Reference + an
// already-computed binding set. Context defaults to EmptyPlanContext
// if nil — convenient for tests that don't depend on planner config.
func NewExpressionRuleCall(ref *expressions.Reference, bindings *matching.PlannerBindings, ctx PlanContext) *ExpressionRuleCall {
	if ctx == nil {
		ctx = EmptyPlanContext()
	}
	return &ExpressionRuleCall{
		Bindings:  bindings,
		Reference: ref,
		Context:   ctx,
	}
}

// NewExpressionRuleCallWithMemo builds a rule-call with a Memo for
// cross-Reference memoization. Used by the Planner's ApplyRulesTask.
func NewExpressionRuleCallWithMemo(ref *expressions.Reference, bindings *matching.PlannerBindings, ctx PlanContext, memo *Memo) *ExpressionRuleCall {
	if ctx == nil {
		ctx = EmptyPlanContext()
	}
	return &ExpressionRuleCall{
		Bindings:  bindings,
		Reference: ref,
		Context:   ctx,
		memo:      memo,
	}
}

// Yield inserts `expr` into the Reference's equivalence class. Returns
// true if the expression was a new member, false if Reference.Insert
// detected a duplicate (matching EqualsWithoutChildren under empty
// alias map). yieldedExps records the call regardless — the rule's
// intent was to yield, even if dedup absorbed the result.
func (c *ExpressionRuleCall) Yield(expr expressions.RelationalExpression) bool {
	if expr == nil {
		panic("ExpressionRuleCall.Yield: nil expression")
	}
	// Validate first, then update state. Reference.Insert panics on
	// nil, so the order matters — without the early check, yieldedExps
	// would have a nil entry leaked before the panic propagated.
	inserted := c.Reference.Insert(expr)
	c.yieldedExps = append(c.yieldedExps, expr)
	return inserted
}

// MemoizeExpression finds or creates a Reference for a sub-expression.
// When a Memo is present (running inside the Planner), this checks if
// an existing Reference already holds a structurally-equivalent
// expression and returns it — enabling cross-Reference sharing.
// Without a Memo (standalone rule testing), falls back to
// expressions.InitialOf(expr).
//
// The current call's Reference (the one the rule is yielding into) is
// excluded from reuse to prevent self-referential cycles. This mirrors
// Java's guard: `Verify.verify(existingReference != this.root)`.
//
// Rules should use this instead of expressions.InitialOf when creating
// child References for yielded expressions. This is how the Cascades
// planner avoids redundant exploration of shared sub-trees.
func (c *ExpressionRuleCall) MemoizeExpression(expr expressions.RelationalExpression) *expressions.Reference {
	if c.memo != nil {
		ref := c.memo.MemoizeExpression(expr)
		if ref == c.Reference {
			return expressions.InitialOf(expr)
		}
		return ref
	}
	return expressions.InitialOf(expr)
}

// Yielded returns the expressions the rule has yielded so far,
// including duplicates that Reference.Insert filtered. Useful for
// rule-firing tests that want to assert on the rule's output without
// reaching into the Reference's member list.
func (c *ExpressionRuleCall) Yielded() []expressions.RelationalExpression {
	return c.yieldedExps
}
