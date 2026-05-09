package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
)

// MatchIntermediateRule is the Cascades rule that matches non-leaf
// query expressions (those with quantifiers) against candidate
// expressions by composing child PartialMatches. For every query
// expression with at least one quantifier, the rule:
//
//  1. Collects the child References from the expression's quantifiers.
//  2. Finds which MatchCandidates have PartialMatches on those child
//     References (seeded by MatchLeafRule or earlier
//     MatchIntermediateRule firings).
//  3. For each such candidate, walks upward through the candidate's
//     Traversal to find parent expressions that reference the
//     candidate-side References from those PartialMatches.
//  4. Attempts a structural match between the query expression and
//     each candidate parent expression, verifying that every quantifier
//     pair is backed by a child PartialMatch.
//  5. On match, creates a new composite PartialMatch and stores it on
//     the query Reference.
//
// This rule propagates matches upward from leaves, enabling multi-level
// expression trees to be matched against candidate (index) expression
// trees. It prepares AdjustMatchRule and physical-implementation rules
// to produce index-scan plans.
//
// Ports Java's
// com.apple.foundationdb.record.query.plan.cascades.rules.MatchIntermediateRule.
// The seed uses ordered quantifier matching (query[i] <-> candidate[i])
// rather than Java's full graph-matching enumeration via
// RelationalExpression.match(). This handles the common case (same
// quantifier count, same order) and will be extended to the full
// combinatorial matcher as needed.
type MatchIntermediateRule struct {
	matcher *ExpressionMatcher[expressions.RelationalExpression]
}

// NewMatchIntermediateRule constructs a MatchIntermediateRule.
func NewMatchIntermediateRule() *MatchIntermediateRule {
	return &MatchIntermediateRule{
		matcher: NewExpressionMatcher[expressions.RelationalExpression]("match_intermediate"),
	}
}

// Matcher returns the binding matcher. Matches any
// RelationalExpression (the non-leaf check is inside OnMatch). Mirrors
// Java's MatchIntermediateRule which returns Optional.empty() from
// getRootOperator().
func (r *MatchIntermediateRule) Matcher() matching.BindingMatcher { return r.matcher }

// OnMatch implements the intermediate matching logic. It collects
// child References, finds candidates with child PartialMatches, walks
// upward through each candidate's Traversal, and attempts structural
// matching at each candidate parent expression.
func (r *MatchIntermediateRule) OnMatch(call *ExpressionRuleCall) {
	expr := call.Bindings.Get(r.matcher).(expressions.RelationalExpression)

	// Only match non-leaf expressions (those with quantifiers).
	quantifiers := expr.GetQuantifiers()
	if len(quantifiers) == 0 {
		return // leaf — handled by MatchLeafRule
	}

	ctx := call.Context
	if ctx == nil {
		return
	}

	// Collect child references from all quantifiers.
	rangesOverRefs := make([]*expressions.Reference, 0, len(quantifiers))
	for _, q := range quantifiers {
		if ref := q.GetRangesOver(); ref != nil {
			rangesOverRefs = append(rangesOverRefs, ref)
		}
	}
	if len(rangesOverRefs) == 0 {
		return
	}

	// Form union of all match candidates that have PartialMatches on
	// any of the child references. This mirrors Java's:
	//   childMatchCandidates.addAll(rangesOverGroup.getMatchCandidates())
	candidateSet := make(map[MatchCandidate]struct{})
	for _, childRef := range rangesOverRefs {
		for _, cand := range GetPartialMatchCandidatesTyped(childRef) {
			candidateSet[cand] = struct{}{}
		}
	}

	// For each candidate, find parent expressions in the candidate's
	// traversal that reference the candidate-side refs from the child
	// PartialMatches. This mirrors Java's
	// MatchCandidate.findReferencingExpressions.
	for candidate := range candidateSet {
		traversal := candidate.GetTraversal()
		if traversal == nil {
			continue
		}

		refToExpressionMap := findReferencingExpressionsForCandidate(
			rangesOverRefs, candidate, traversal,
		)

		// For each (candidateRef, candidateExpr) pair, attempt to
		// match the query expression against the candidate expression.
		for candidateRef, candidateExprs := range refToExpressionMap {
			for _, candidateExpr := range candidateExprs {
				matchIntermediateWithCandidate(
					call, expr, candidate, candidateRef, candidateExpr,
				)
			}
		}
	}
}

// findReferencingExpressionsForCandidate implements Java's
// MatchCandidate.findReferencingExpressions: for each query-side child
// reference, retrieves the PartialMatches for the given candidate, then
// for each PartialMatch walks upward from the candidate-side reference
// to find the parent (ref, expr) pairs in the traversal.
//
// Returns a map from candidate Reference to the expressions that own
// quantifiers ranging over it.
func findReferencingExpressionsForCandidate(
	queryChildRefs []*expressions.Reference,
	candidate MatchCandidate,
	traversal *Traversal,
) map[*expressions.Reference][]expressions.RelationalExpression {
	result := make(map[*expressions.Reference][]expressions.RelationalExpression)

	type pairKey struct {
		ref  *expressions.Reference
		expr expressions.RelationalExpression
	}
	seen := make(map[pairKey]bool)

	for _, queryChildRef := range queryChildRefs {
		childMatches := GetPartialMatchesForCandidate(queryChildRef, candidate)
		for _, pm := range childMatches {
			pmi, ok := pm.(*PartialMatchImpl)
			if !ok {
				continue
			}
			candidateChildRef := pmi.GetCandidateRef()
			for _, parent := range traversal.GetParentRefPairs(candidateChildRef) {
				key := pairKey{ref: parent.ref, expr: parent.expr}
				if seen[key] {
					continue
				}
				seen[key] = true
				result[parent.ref] = append(result[parent.ref], parent.expr)
			}
		}
	}

	return result
}

// matchIntermediateWithCandidate attempts to match a query expression
// against a candidate expression at the intermediate (non-leaf) level.
// Checks structural equality of the expressions and verifies that
// every quantifier pair is backed by a child PartialMatch.
//
// Seed implementation: ordered quantifier matching (queryQs[i] <->
// candidateQs[i]). Java's full implementation uses
// RelationalExpression.match() which enumerates all valid quantifier
// permutations; the seed handles the common case of same-order,
// same-count quantifiers.
func matchIntermediateWithCandidate(
	call *ExpressionRuleCall,
	queryExpr expressions.RelationalExpression,
	candidate MatchCandidate,
	candidateRef *expressions.Reference,
	candidateExpr expressions.RelationalExpression,
) {
	queryQs := queryExpr.GetQuantifiers()
	candidateQs := candidateExpr.GetQuantifiers()

	if len(queryQs) != len(candidateQs) {
		return
	}

	// Structural equality check at this level (ignoring children).
	exprAliasMap := expressions.EmptyAliasMap()
	if !queryExpr.EqualsWithoutChildren(candidateExpr, exprAliasMap) {
		return
	}

	// Build the alias map from quantifier bindings and verify that
	// each quantifier pair is backed by a child PartialMatch.
	aliasBuilder := NewAliasMapBuilder()

	for i, queryQ := range queryQs {
		queryChildRef := queryQ.GetRangesOver()
		candidateChildRef := candidateQs[i].GetRangesOver()

		// Find a PartialMatch on queryChildRef for this candidate
		// whose candidate-side ref matches candidateChildRef.
		found := false
		childMatches := GetPartialMatchesForCandidate(queryChildRef, candidate)
		for _, pm := range childMatches {
			pmi, ok := pm.(*PartialMatchImpl)
			if !ok {
				continue
			}
			if pmi.GetCandidateRef() == candidateChildRef {
				// Incorporate the child's alias mappings.
				aliasBuilder.PutAll(pmi.GetBoundAliasMap())
				found = true
				break
			}
		}
		if !found {
			return
		}

		// Map the query quantifier's alias to the candidate
		// quantifier's alias.
		aliasBuilder.Put(queryQ.GetAlias(), candidateQs[i].GetAlias())
	}

	// All quantifiers matched — create a composite PartialMatch.
	boundAliasMap := aliasBuilder.Build()

	mi := NewRegularMatchInfo(
		nil,                    // parameterBindingMap
		boundAliasMap,          // bindingAliasMap
		nil,                    // predicateMap
		nil,                    // matchedOrderingParts
		nil,                    // maxMatchMap
		EmptyGroupByMappings(), // groupByMappings
		nil,                    // rollUpToGroupingValues
		nil,                    // additionalPlanConstraint
	)

	pm := NewPartialMatch(
		boundAliasMap,
		candidate,
		call.Reference,
		queryExpr,
		candidateRef,
		mi,
	)
	AddPartialMatchForCandidate(call.Reference, candidate, pm)
}

var _ ExpressionRule = (*MatchIntermediateRule)(nil)
