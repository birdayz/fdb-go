package cascades

import (
	"reflect"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
)

// ExpressionMatcher type-asserts a candidate against a specific
// RelationalExpression concrete type T and binds the host on match.
// Counterpart to the existing `predicateMatcher[T]` for QueryPredicate
// rules — same shape, different type bound.
//
// Used by RelationalExpression-shaped rules (FilterMergeRule,
// PushFilterThroughDistinctRule, and the rest of the rule_*.go files).
type ExpressionMatcher[T expressions.RelationalExpression] struct {
	rootType string
}

// RootType returns the rule's debug-friendly root identifier.
func (m *ExpressionMatcher[T]) RootType() string { return m.rootType }

// RootOperator returns the one concrete type BindMatches admits — computed
// from the type parameter, so it can never drift from the type assert.
// Implements matching.RootOperatorMatcher for the planner's rule index.
//
// An INTERFACE type parameter (ExpressionMatcher[RelationalExpression] —
// the match-anything shape MatchLeafRule / MatchIntermediateRule use)
// returns nil: the assert admits every implementor, so the rule belongs in
// the index's always bucket. Returning the interface type instead would
// bucket the rule under a key no concrete expression ever looks up —
// silently disabling the matching infrastructure (Java models this as
// PlannerRule.getRootOperator returning Optional.empty()).
func (m *ExpressionMatcher[T]) RootOperator() reflect.Type {
	t := reflect.TypeFor[T]()
	if t.Kind() == reflect.Interface {
		return nil
	}
	return t
}

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

// FireExpressionRule is a standalone driver for testing
// ExpressionRules. Matches the rule's pattern against every member of
// `ref` and invokes OnMatch for each successful match. Yields are
// inserted into `ref` via the ExpressionRuleCall's Reference; the
// returned slice is the rule's intent (Yielded()).
//
// Production rule driving lives in the planner's task stack
// (unified_tasks.go); this helper is the testable entry point — same
// pattern as FireRule for predicate/value rules.
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
		all = append(all, fireExprRuleOnMember(rule, matcher, ref, member, ctx, memo)...)

		// ChildrenAsSet permutation: for expressions whose children are
		// order-independent (SelectExpression with INNER or CROSS joins),
		// also fire the rule with quantifiers swapped so join rules
		// explore both outer/inner assignments. The swapped expression is
		// ephemeral — it is NOT inserted into the memo.
		//
		// Only swap when the first two quantifiers are both ForEach
		// (a real join). Existential quantifiers indicate semi-joins
		// (EXISTS subqueries) where quantifier ordering is semantic.
		if sel, ok := member.(*expressions.SelectExpression); ok && sel.ChildrenAsSet() {
			qs := sel.GetQuantifiers()
			if len(qs) >= 2 && sel.GetJoinType() != expressions.JoinLeftOuter &&
				qs[0].Kind() == expressions.QuantifierForEach &&
				qs[1].Kind() == expressions.QuantifierForEach {
				swapped := sel.WithSwappedQuantifiers()
				all = append(all, fireExprRuleOnMember(rule, matcher, ref, swapped, ctx, memo)...)
			}
		}
	}
	return all
}

// fireExprRuleOnMember runs a single expression rule against a single
// member, returning yielded expressions. Extracted to avoid duplication
// between normal and ChildrenAsSet-permuted firing.
func fireExprRuleOnMember(
	rule ExpressionRule,
	matcher matching.BindingMatcher,
	ref *expressions.Reference,
	member expressions.RelationalExpression,
	ctx PlanContext,
	memo *Memo,
) []expressions.RelationalExpression {
	matches := matcher.BindMatches(matching.NewBindings(), member)
	var out []expressions.RelationalExpression
	for _, b := range matches {
		var call *ExpressionRuleCall
		if memo != nil {
			call = NewExpressionRuleCallWithMemo(ref, b, ctx, memo)
		} else {
			call = NewExpressionRuleCall(ref, b, ctx)
		}
		rule.OnMatch(call)
		out = append(out, call.Yielded()...)
	}
	return out
}
