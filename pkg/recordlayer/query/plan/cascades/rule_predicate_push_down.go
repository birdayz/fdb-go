package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// PredicatePushDownRule pushes predicates from an outer SelectExpression
// into its child quantifiers when the predicate only references a
// single child's aliases. This is the generic predicate push-down rule
// intended for the REWRITING phase — it handles arbitrary
// SelectExpression shapes, unlike the specific Push*Through* rules
// which target Filter/Projection/Sort/etc.
//
// Algorithm:
//  1. Match SelectExpression.
//  2. For each ForEach quantifier, identify predicates that can be
//     pushed — those whose correlation set has no overlap with any
//     OTHER quantifier's alias.
//  3. For each pushable-predicate + quantifier pair, visit the child
//     expression through the quantifier's Reference and create a new
//     expression with the predicate absorbed or pushed through.
//  4. Build a new outer SelectExpression with the remaining (non-pushed)
//     predicates and the rewritten quantifiers.
//
// Guard: skips SelectExpressions containing Existential quantifiers
// (same guard as NormalizePredicatesRule). Existential quantifiers have
// different correlation semantics — pushing predicates into them would
// change the query's meaning.
//
// Convergence: each firing strictly reduces the set of pushable
// predicates in the outer SelectExpression. A SelectExpression with no
// pushable predicates causes zero yields.
//
// Ports Java's PredicatePushDownRule (ExplorationCascadesRule, 444 LOC).
type PredicatePushDownRule struct {
	matcher matching.BindingMatcher
}

func NewPredicatePushDownRule() *PredicatePushDownRule {
	return &PredicatePushDownRule{
		matcher: NewExpressionMatcher[*expressions.SelectExpression]("predicate_push_down"),
	}
}

func (r *PredicatePushDownRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *PredicatePushDownRule) OnMatch(call *ExpressionRuleCall) {
	sel := matching.Get[*expressions.SelectExpression](call.Bindings, r.matcher)
	quantifiers := sel.GetQuantifiers()

	// Guard: don't push predicates into SelectExpressions containing
	// Existential quantifiers. Same guard as NormalizePredicatesRule.
	for _, q := range quantifiers {
		if q.Kind() == expressions.QuantifierExistential {
			return
		}
	}

	allPredicates := sel.GetPredicates()
	if len(allPredicates) == 0 {
		return
	}

	// For each ForEach quantifier, try to push predicates into it.
	// We iterate quantifiers one at a time: for each, we identify
	// predicates that reference ONLY that quantifier (and no other
	// sibling quantifier). Java's rule matches on a single ForEach
	// quantifier at a time (via the matcher) and fires once per
	// quantifier; Go's rule iterates all quantifiers in one pass.
	for qIdx, pushQ := range quantifiers {
		if pushQ.Kind() != expressions.QuantifierForEach {
			continue
		}
		if pushQ.IsNullOnEmpty() {
			continue
		}

		// Compute the set of "other" quantifier aliases.
		otherAliases := map[values.CorrelationIdentifier]struct{}{}
		for j, q := range quantifiers {
			if j == qIdx {
				continue
			}
			otherAliases[q.GetAlias()] = struct{}{}
		}

		// Partition predicates: pushable vs fixed.
		var pushable, fixed []predicates.QueryPredicate
		for _, pred := range allPredicates {
			correlated := predicates.GetCorrelatedToOfPredicate(pred)
			canPush := true
			for alias := range correlated {
				if _, isOther := otherAliases[alias]; isOther {
					canPush = false
					break
				}
			}
			if canPush {
				pushable = append(pushable, pred)
			} else {
				fixed = append(fixed, pred)
			}
		}

		if len(pushable) == 0 {
			continue
		}

		// Try to push the pushable predicates into/through the child
		// expression. Visit the child Reference's members.
		childRef := pushQ.GetRangesOver()
		if childRef == nil {
			continue
		}

		var newBelowExpressions []expressions.RelationalExpression
		for _, member := range childRef.AllMembers() {
			pushed := pushPredicateToExpression(call, pushable, pushQ, member)
			if pushed != nil {
				newBelowExpressions = append(newBelowExpressions, pushed)
			}
		}

		if len(newBelowExpressions) == 0 {
			continue
		}

		// Memoize the new below expressions into a new Reference.
		var newChildRef *expressions.Reference
		if len(newBelowExpressions) == 1 {
			newChildRef = call.MemoizeExpression(newBelowExpressions[0])
		} else {
			newChildRef = expressions.InitialOf(newBelowExpressions[0])
			for i := 1; i < len(newBelowExpressions); i++ {
				newChildRef.Insert(newBelowExpressions[i])
			}
		}

		// Build new quantifier with the same alias but ranging over
		// the new child Reference.
		newPushQ := expressions.NamedForEachQuantifier(pushQ.GetAlias(), newChildRef)

		// Build the new quantifier list, replacing the push quantifier.
		newQuantifiers := make([]expressions.Quantifier, len(quantifiers))
		for i, q := range quantifiers {
			if i == qIdx {
				newQuantifiers[i] = newPushQ
			} else {
				newQuantifiers[i] = q
			}
		}

		// Build the new SelectExpression with the remaining (fixed) predicates.
		newSel := expressions.NewSelectExpressionWithJoinType(
			sel.GetResultValue(),
			newQuantifiers,
			fixed,
			sel.GetSourceAliases(),
			sel.GetJoinType(),
		)
		call.Yield(newSel)
		return // One quantifier per rule firing, matching Java's behavior.
	}
}

// pushPredicateToExpression is the Go equivalent of Java's PushToVisitor.
// It visits the child expression and returns a new expression with the
// predicates pushed in, or nil if the expression type doesn't support
// predicate push-down.
func pushPredicateToExpression(
	call *ExpressionRuleCall,
	originalPredicates []predicates.QueryPredicate,
	pushQuantifier expressions.Quantifier,
	belowExpression expressions.RelationalExpression,
) expressions.RelationalExpression {
	switch expr := belowExpression.(type) {
	case *expressions.LogicalFilterExpression:
		return pushIntoLogicalFilter(originalPredicates, pushQuantifier, expr)
	case *expressions.SelectExpression:
		return pushIntoSelect(originalPredicates, pushQuantifier, expr)
	case *expressions.LogicalUnionExpression:
		return pushThroughUnion(call, originalPredicates, pushQuantifier, expr)
	case *expressions.LogicalSortExpression:
		return pushThroughSort(call, originalPredicates, pushQuantifier, expr)
	case *expressions.LogicalDistinctExpression:
		return pushThroughDistinct(call, originalPredicates, pushQuantifier, expr)
	case *expressions.LogicalUniqueExpression:
		return pushThroughUnique(call, originalPredicates, pushQuantifier, expr)
	default:
		// By default, we cannot push things down. Return nil.
		return nil
	}
}

// pushIntoLogicalFilter absorbs predicates into a LogicalFilterExpression
// by combining the original predicates (rebased to the filter's inner
// alias) with the filter's existing predicates. Returns a new
// SelectExpression. Ports Java's PushToVisitor.visitLogicalFilterExpression.
func pushIntoLogicalFilter(
	originalPredicates []predicates.QueryPredicate,
	pushQuantifier expressions.Quantifier,
	filter *expressions.LogicalFilterExpression,
) expressions.RelationalExpression {
	inner := filter.GetInner()
	if inner.Kind() != expressions.QuantifierForEach {
		return nil
	}

	// Rebase: pushQuantifier.alias -> inner.alias
	aliasMap := values.AliasMap{
		pushQuantifier.GetAlias(): inner.GetAlias(),
	}

	// Combine: existing filter predicates + rebased original predicates.
	newPredicates := make([]predicates.QueryPredicate, 0, len(filter.GetPredicates())+len(originalPredicates))
	newPredicates = append(newPredicates, filter.GetPredicates()...)
	for _, p := range originalPredicates {
		newPredicates = append(newPredicates, predicates.RebasePredicate(p, aliasMap))
	}

	return expressions.NewSelectExpression(
		inner.GetFlowedObjectValue(),
		[]expressions.Quantifier{inner},
		newPredicates,
	)
}

// pushIntoSelect absorbs predicates into a SelectExpression by
// translating them to reference the select's result value and combining
// with the select's existing predicates. Returns a new SelectExpression.
// Ports Java's PushToVisitor.visitSelectExpression.
func pushIntoSelect(
	originalPredicates []predicates.QueryPredicate,
	pushQuantifier expressions.Quantifier,
	selectExpr *expressions.SelectExpression,
) expressions.RelationalExpression {
	// Build a TranslationMap: pushQuantifier.alias -> selectExpr.resultValue.
	resultValue := selectExpr.GetResultValue()
	tmBuilder := NewTranslationMapBuilder()
	tmBuilder.When(pushQuantifier.GetAlias()).Then(func(_ values.CorrelationIdentifier, _ values.LeafValue) values.Value {
		return resultValue
	})
	tm := tmBuilder.Build()

	// Combine: existing select predicates + translated original predicates.
	newPredicates := make([]predicates.QueryPredicate, 0, len(selectExpr.GetPredicates())+len(originalPredicates))
	newPredicates = append(newPredicates, selectExpr.GetPredicates()...)
	for _, p := range originalPredicates {
		newPredicates = append(newPredicates, translatePredicateCorrelations(p, tm))
	}

	return expressions.NewSelectExpressionWithJoinType(
		selectExpr.GetResultValue(),
		selectExpr.GetQuantifiers(),
		newPredicates,
		selectExpr.GetSourceAliases(),
		selectExpr.GetJoinType(),
	)
}

// pushOverChild creates a new SelectExpression wrapping the child
// quantifier with the pushed-down predicates. Used when pushing through
// expressions that don't directly absorb predicates. Returns a new
// ForEach quantifier ranging over the new SelectExpression.
// Ports Java's PushToVisitor.pushOverChild.
func pushOverChild(
	call *ExpressionRuleCall,
	originalPredicates []predicates.QueryPredicate,
	pushQuantifier expressions.Quantifier,
	child expressions.Quantifier,
) expressions.Quantifier {
	// Rebase: pushQuantifier.alias -> child.alias
	aliasMap := values.AliasMap{
		pushQuantifier.GetAlias(): child.GetAlias(),
	}

	newPredicates := make([]predicates.QueryPredicate, len(originalPredicates))
	for i, p := range originalPredicates {
		newPredicates[i] = predicates.RebasePredicate(p, aliasMap)
	}

	newSelect := expressions.NewSelectExpression(
		child.GetFlowedObjectValue(),
		[]expressions.Quantifier{child},
		newPredicates,
	)
	return expressions.ForEachQuantifier(call.MemoizeExpression(newSelect))
}

// pushThroughUnion pushes predicates through a LogicalUnionExpression by
// creating a new SelectExpression over each union leg with the pushed
// predicates. Ports Java's PushToVisitor.visitLogicalUnionExpression.
func pushThroughUnion(
	call *ExpressionRuleCall,
	originalPredicates []predicates.QueryPredicate,
	pushQuantifier expressions.Quantifier,
	union *expressions.LogicalUnionExpression,
) expressions.RelationalExpression {
	qs := union.GetQuantifiers()
	newChildren := make([]expressions.Quantifier, len(qs))
	for i, q := range qs {
		if q.Kind() != expressions.QuantifierForEach {
			return nil
		}
		newChildren[i] = pushOverChild(call, originalPredicates, pushQuantifier, q)
	}
	return expressions.NewLogicalUnionExpression(newChildren)
}

// pushThroughSort pushes predicates through a LogicalSortExpression by
// creating a new SelectExpression below the sort's single child.
// Ports Java's PushToVisitor.visitLogicalSortExpression.
func pushThroughSort(
	call *ExpressionRuleCall,
	originalPredicates []predicates.QueryPredicate,
	pushQuantifier expressions.Quantifier,
	sort *expressions.LogicalSortExpression,
) expressions.RelationalExpression {
	inner := sort.GetInner()
	if inner.Kind() != expressions.QuantifierForEach {
		return nil
	}
	newChild := pushOverChild(call, originalPredicates, pushQuantifier, inner)
	return expressions.NewLogicalSortExpression(sort.GetSortKeys(), newChild)
}

// pushThroughDistinct pushes predicates through a LogicalDistinctExpression
// by creating a new SelectExpression below the distinct's single child.
// Ports Java's PushToVisitor.visitLogicalDistinctExpression.
func pushThroughDistinct(
	call *ExpressionRuleCall,
	originalPredicates []predicates.QueryPredicate,
	pushQuantifier expressions.Quantifier,
	distinct *expressions.LogicalDistinctExpression,
) expressions.RelationalExpression {
	inner := distinct.GetInner()
	if inner.Kind() != expressions.QuantifierForEach {
		return nil
	}
	newChild := pushOverChild(call, originalPredicates, pushQuantifier, inner)
	return expressions.NewLogicalDistinctExpression(newChild)
}

// pushThroughUnique pushes predicates through a LogicalUniqueExpression
// by creating a new SelectExpression below the unique's single child.
// Ports Java's PushToVisitor.visitLogicalUniqueExpression.
func pushThroughUnique(
	call *ExpressionRuleCall,
	originalPredicates []predicates.QueryPredicate,
	pushQuantifier expressions.Quantifier,
	unique *expressions.LogicalUniqueExpression,
) expressions.RelationalExpression {
	inner := unique.GetInner()
	if inner.Kind() != expressions.QuantifierForEach {
		return nil
	}
	newChild := pushOverChild(call, originalPredicates, pushQuantifier, inner)
	return expressions.NewLogicalUniqueExpression(newChild)
}

var _ ExpressionRule = (*PredicatePushDownRule)(nil)
