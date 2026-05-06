package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
)

// ExpressionMatcher type-asserts a candidate against a specific
// RelationalExpression concrete type T and binds the host on match.
// Counterpart to the existing `predicateMatcher[T]` for QueryPredicate
// rules — same shape, different type bound.
//
// Used by RelationalExpression-shaped rules (FilterMergeRule below
// is the seed consumer).
type ExpressionMatcher[T expressions.RelationalExpression] struct {
	rootType string
}

// RootType returns the rule's debug-friendly root identifier.
func (m *ExpressionMatcher[T]) RootType() string { return m.rootType }

// BindMatches type-asserts `in` to T; on success binds m → in in the
// outer bindings and returns one new binding set. On failure returns
// nil.
func (m *ExpressionMatcher[T]) BindMatches(outer *matching.PlannerBindings, in any) []*matching.PlannerBindings {
	if _, ok := in.(T); !ok {
		return nil
	}
	return []*matching.PlannerBindings{outer.Bind(m, in)}
}

// NewExpressionMatcher constructs a typed matcher for the given
// RelationalExpression subtype. Each call returns a distinct
// allocation so pointer-identity comparisons stay distinct across
// rule instances.
func NewExpressionMatcher[T expressions.RelationalExpression](rootType string) *ExpressionMatcher[T] {
	return &ExpressionMatcher[T]{rootType: rootType}
}

// ExpressionRule is the transform interface for RelationalExpression-
// shaped rules. Counterpart to CascadesRule for the QueryPredicate /
// Value-shaped rules. Each impl provides:
//
//   - Matcher: pattern the rule fires on (typically an
//     ExpressionMatcher for the rule's root expression type).
//   - OnMatch: rule body — reads call.Bindings via Get[T] / Get and
//     calls call.Yield(replacement) for each rewritten expression.
type ExpressionRule interface {
	Matcher() matching.BindingMatcher
	OnMatch(call *ExpressionRuleCall)
}

// FireExpressionRule is the seed-time driver for testing
// ExpressionRules. Matches the rule's pattern against every member of
// `ref` and invokes OnMatch for each successful match. Yields are
// inserted into `ref` via the ExpressionRuleCall's Reference; the
// returned slice is the rule's intent (Yielded()).
//
// Production rule driving lives in the CascadesPlanner task stack
// (Phase 4.6); this helper exists so the seed has a testable entry
// point — same pattern as FireRule for predicate/value rules.
func FireExpressionRule(rule ExpressionRule, ref *expressions.Reference) []expressions.RelationalExpression {
	return FireExpressionRuleWithMemo(rule, ref, EmptyPlanContext(), nil)
}

// FireExpressionRuleWithMemo is like FireExpressionRule but passes a
// PlanContext and Memo to the rule call, enabling cross-Reference
// memoization when running inside the Planner.
func FireExpressionRuleWithMemo(rule ExpressionRule, ref *expressions.Reference, ctx PlanContext, memo *Memo) []expressions.RelationalExpression {
	matcher := rule.Matcher()
	var all []expressions.RelationalExpression
	for _, member := range ref.Members() {
		matches := matcher.BindMatches(matching.NewBindings(), member)
		for _, b := range matches {
			var call *ExpressionRuleCall
			if memo != nil {
				call = NewExpressionRuleCallWithMemo(ref, b, ctx, memo)
			} else {
				call = NewExpressionRuleCall(ref, b, ctx)
			}
			rule.OnMatch(call)
			all = append(all, call.Yielded()...)
		}
	}
	return all
}
