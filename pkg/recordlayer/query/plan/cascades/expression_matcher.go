// Portions derived from FoundationDB Record Layer (PlannerRule.java),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

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
	rootType       string
	rootPredicate  func(T) bool
	inputPredicate func(T) bool
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
	if !m.MatchesRoot(in) || !m.MatchesInputs(in) {
		return nil
	}
	return []*matching.PlannerBindings{outer.Bind(m, in)}
}

// MatchesRoot checks type and immutable root predicates without binding children.
func (m *ExpressionMatcher[T]) MatchesRoot(in any) bool {
	expr, ok := in.(T)
	return ok && (m.rootPredicate == nil || m.rootPredicate(expr))
}

// WithRootPredicate constructs a matcher restricted by immutable root fields.
// The predicate must not inspect mutable child-reference contents or constraints.
func (m *ExpressionMatcher[T]) WithRootPredicate(predicate func(T) bool) *ExpressionMatcher[T] {
	cp := *m
	cp.rootPredicate = func(expr T) bool {
		return (m.rootPredicate == nil || m.rootPredicate(expr)) && (predicate == nil || predicate(expr))
	}
	return &cp
}

// MatchesInputs tests the current child members, without caching their state.
func (m *ExpressionMatcher[T]) MatchesInputs(in any) bool {
	expr, ok := in.(T)
	return ok && (m.inputPredicate == nil || m.inputPredicate(expr))
}

// WithInputPredicate restricts matching by immediate child members. It must not
// read descendants or planner constraints, which may change independently.
func (m *ExpressionMatcher[T]) WithInputPredicate(predicate func(T) bool) *ExpressionMatcher[T] {
	cp := *m
	cp.inputPredicate = func(expr T) bool {
		return (m.inputPredicate == nil || m.inputPredicate(expr)) && (predicate == nil || predicate(expr))
	}
	return &cp
}

func referenceHasMemberOfType[T expressions.RelationalExpression](ref *expressions.Reference) bool {
	if ref != nil {
		for _, member := range ref.AllMembers() {
			if _, ok := member.(T); ok {
				return true
			}
		}
	}
	return false
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
func FireExpressionRule(rule ExpressionRule, ref *expressions.Reference) ([]expressions.RelationalExpression, error) {
	return FireExpressionRuleWithMemo(rule, ref, EmptyPlanContext(), nil)
}

// FireExpressionRuleWithMemo is like FireExpressionRule but passes a
// PlanContext and Memo to the rule call, enabling cross-Reference
// memoization when running inside the Planner.
func FireExpressionRuleWithMemo(rule ExpressionRule, ref *expressions.Reference, ctx PlanContext, memo *Memo) ([]expressions.RelationalExpression, error) {
	matcher := rule.Matcher()
	var all []expressions.RelationalExpression
	for _, member := range ref.Members() {
		yielded, err := fireExprRuleOnMember(rule, matcher, ref, member, ctx, memo)
		if err != nil {
			return nil, err
		}
		all = append(all, yielded...)

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
				yielded, err := fireExprRuleOnMember(rule, matcher, ref, swapped, ctx, memo)
				if err != nil {
					return nil, err
				}
				all = append(all, yielded...)
			}
		}
	}
	return all, nil
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
) ([]expressions.RelationalExpression, error) {
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
		if err := call.Err(); err != nil {
			return nil, err
		}
		yielded := call.Yielded()
		var batch *preparedReferenceBatch
		if len(yielded) > 0 {
			intents := make([]referenceMemberIntent, len(yielded))
			for i, y := range yielded {
				intents[i] = referenceMemberIntent{set: expressions.ReferenceExploratoryMembers, expression: y}
			}
			prepared, err := prepareReferenceMemberBatch(ref, intents)
			if err != nil {
				return nil, err
			}
			batch = prepared
		}
		// Same boundary as the task driver: after EVERY fallible step, and
		// before the parent members land, so a parent still lands over complete
		// children. A clear Err only says the rule BODY succeeded — prepare can
		// still reject the batch, and an insert published above it survives
		// that rejection, which is the leak staging exists to close.
		call.CommitStagedInserts()
		if batch != nil {
			if err := batch.commit(); err != nil {
				return nil, err
			}
		}
		for _, y := range yielded {
			if memo != nil {
				memo.Integrate(ref, y)
			}
			out = append(out, y)
		}
	}
	return out, nil
}
