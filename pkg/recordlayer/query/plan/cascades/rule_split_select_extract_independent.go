package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/combinatorics"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// SplitSelectExtractIndependentQuantifiersRule splits a SelectExpression
// into two when one or more ForEach quantifiers ranging over
// ExplodeExpressions are fully independent — not correlated to any
// other quantifier in the SelectExpression.
//
// The independent quantifiers are extracted into an outer
// SelectExpression; the correlated quantifiers remain in the inner.
// This is safe and often beneficial because the extracted quantifiers
// range over ExplodeExpressions (bounded cardinality), so the outer
// cross-product is small. Star-join optimisations benefit from this
// partitioning.
//
// Ports Java's SplitSelectExtractIndependentQuantifiersRule (184 LOC).
//
// Algorithm:
//  1. Match a SelectExpression with at least one ForEach quantifier
//     ranging over an ExplodeExpression.
//  2. Build a PartiallyOrderedSet of all quantifier aliases, with
//     dependency edges derived from correlation analysis (quantifier A
//     depends on alias B if A's inner expression is correlated to B).
//  3. Compute the eligible set — aliases with no dependencies (in-degree
//     zero in the partial order). Only explode-backed ForEach quantifiers
//     that are eligible are candidates for extraction.
//  4. Partition quantifiers into lower (non-eligible or non-explode) and
//     upper (eligible explode). Both partitions must be non-empty, and
//     the lower must contain at least one ForEach.
//  5. Guard: skip if the SelectExpression is "simple" — no predicates
//     and the result value doesn't reference any explode alias.
//  6. Yield: lower quantifiers + all predicates go into the inner
//     SelectExpression; the outer gets the upper quantifiers plus a
//     new ForEach over the inner, with the inner's flowed-object value
//     as its result.
//
// Convergence: each firing strictly reduces the number of quantifiers
// in the inner SelectExpression. The isSimpleSelect guard prevents
// infinite expansion when the result is already trivially structured.
type SplitSelectExtractIndependentQuantifiersRule struct {
	matcher matching.BindingMatcher
}

func NewSplitSelectExtractIndependentQuantifiersRule() *SplitSelectExtractIndependentQuantifiersRule {
	return &SplitSelectExtractIndependentQuantifiersRule{
		matcher: NewExpressionMatcher[*expressions.SelectExpression]("split_select_extract_independent"),
	}
}

func (r *SplitSelectExtractIndependentQuantifiersRule) Matcher() matching.BindingMatcher {
	return r.matcher
}

func (r *SplitSelectExtractIndependentQuantifiersRule) OnMatch(call *ExpressionRuleCall) {
	sel := matching.Get[*expressions.SelectExpression](call.Bindings, r.matcher)
	quantifiers := sel.GetQuantifiers()

	// Step 1: Identify ForEach quantifiers ranging over ExplodeExpressions.
	explodeAliases := map[values.CorrelationIdentifier]struct{}{}
	for _, q := range quantifiers {
		if q.Kind() != expressions.QuantifierForEach {
			continue
		}
		ref := q.GetRangesOver()
		if ref == nil {
			continue
		}
		if getExplodeExpressionFromRef(ref) != nil {
			explodeAliases[q.GetAlias()] = struct{}{}
		}
	}
	if len(explodeAliases) == 0 {
		return
	}

	// Step 2: Guard against simple selects to prevent inductive explosion.
	if isSimpleSelectForSplit(sel, explodeAliases) {
		return
	}

	// Step 3: Build a PartiallyOrderedSet over all quantifier aliases,
	// with dependency edges from correlation analysis.
	allAliases := map[values.CorrelationIdentifier]struct{}{}
	for _, q := range quantifiers {
		allAliases[q.GetAlias()] = struct{}{}
	}

	builder := combinatorics.NewBuilder[values.CorrelationIdentifier]()
	for _, q := range quantifiers {
		alias := q.GetAlias()
		builder.Add(alias)
		correlatedTo := quantifierCorrelationSet(q)
		for dep := range correlatedTo {
			if _, ok := allAliases[dep]; ok {
				builder.AddDependency(alias, dep)
			}
		}
	}
	aliasesPartialOrder := builder.Build()

	// Step 4: Compute eligible set — aliases with zero in-degree.
	eligibleSet := aliasesPartialOrder.EligibleSet()
	eligible := eligibleSet.EligibleElements()

	// Step 5: Partition quantifiers.
	var lowerQuantifiers, upperQuantifiers []expressions.Quantifier
	for _, q := range quantifiers {
		alias := q.GetAlias()
		_, isExplode := explodeAliases[alias]
		_, isEligible := eligible[alias]
		if isExplode && isEligible {
			upperQuantifiers = append(upperQuantifiers, q)
		} else {
			lowerQuantifiers = append(lowerQuantifiers, q)
		}
	}

	// Both partitions must be non-empty.
	if len(lowerQuantifiers) == 0 || len(upperQuantifiers) == 0 {
		return
	}

	// The lower partition must contain at least one ForEach quantifier.
	hasForEach := false
	for _, q := range lowerQuantifiers {
		if q.Kind() == expressions.QuantifierForEach {
			hasForEach = true
			break
		}
	}
	if !hasForEach {
		return
	}

	// Convergence guard: if any predicate references an alias from the
	// upper partition, the split would create a lower SelectExpression
	// that's correlated to an upper quantifier. In Go (where this rule
	// and SelectMergeRule run in the same phase), that triggers a
	// split-merge cycle. Java avoids this via explore/implement phase
	// separation. The guard is also semantically correct: predicates
	// referencing the explode alias mean the quantifiers interact
	// through the WHERE clause, not just the FROM-list correlation
	// order, so they shouldn't be separated.
	upperAliases := map[values.CorrelationIdentifier]struct{}{}
	for _, q := range upperQuantifiers {
		upperAliases[q.GetAlias()] = struct{}{}
	}
	for _, pred := range sel.GetPredicates() {
		predCorr := predicates.GetCorrelatedToOfPredicate(pred)
		for alias := range predCorr {
			if _, ok := upperAliases[alias]; ok {
				return
			}
		}
	}

	// Step 6: Build the new expressions.

	// Inner SelectExpression: non-eligible quantifiers + all predicates
	// + the original result value.
	lowerSelect := expressions.NewSelectExpression(
		sel.GetResultValue(),
		lowerQuantifiers,
		sel.GetPredicates(),
	)
	lowerRef := call.MemoizeExpression(lowerSelect)
	lowerQ := expressions.ForEachQuantifier(lowerRef)

	// Outer SelectExpression: eligible explode quantifiers + the new
	// ForEach over the inner + no predicates. Result = inner's flowed
	// object value.
	outerQuantifiers := make([]expressions.Quantifier, 0, len(upperQuantifiers)+1)
	outerQuantifiers = append(outerQuantifiers, upperQuantifiers...)
	outerQuantifiers = append(outerQuantifiers, lowerQ)

	outerSelect := expressions.NewSelectExpression(
		lowerQ.GetFlowedObjectValue(),
		outerQuantifiers,
		nil, // no predicates
	)

	call.Yield(outerSelect)
}

// isSimpleSelectForSplit returns true when the SelectExpression is
// trivially structured: no predicates and the result value doesn't
// reference any of the explode aliases. Splitting such an expression
// would not reduce complexity and would cause infinite inductive
// expansion.
//
// Mirrors Java's isSimpleSelect().
func isSimpleSelectForSplit(sel *expressions.SelectExpression, explodeAliases map[values.CorrelationIdentifier]struct{}) bool {
	if len(sel.GetPredicates()) > 0 {
		return false
	}
	resultCorrelations := values.GetCorrelatedToOfValue(sel.GetResultValue())
	for alias := range resultCorrelations {
		if _, ok := explodeAliases[alias]; ok {
			return false
		}
	}
	return true
}

// quantifierCorrelationSet computes the correlation set of a
// quantifier by walking the inner expression tree. Since Go's
// Quantifier.GetCorrelatedTo() currently returns the empty set (the
// comment says "revisit when multi-level rules port"), we compute it
// here by examining the Reference's members.
//
// For each member of the quantifier's Reference, we collect
// GetCorrelatedToWithoutChildren() and recursively descend into
// the member's quantifiers' References.
func quantifierCorrelationSet(q expressions.Quantifier) map[values.CorrelationIdentifier]struct{} {
	ref := q.GetRangesOver()
	if ref == nil {
		return map[values.CorrelationIdentifier]struct{}{}
	}
	result := map[values.CorrelationIdentifier]struct{}{}
	for _, member := range ref.AllMembers() {
		expressionCorrelationSet(member, result)
	}
	return result
}

// expressionCorrelationSet collects all correlation identifiers that an
// expression and its transitive children depend on. Accumulates into
// `out` to avoid repeated allocation.
func expressionCorrelationSet(expr expressions.RelationalExpression, out map[values.CorrelationIdentifier]struct{}) {
	// Collect this expression's own correlations.
	for alias := range expr.GetCorrelatedToWithoutChildren() {
		out[alias] = struct{}{}
	}
	// Recurse into children (quantifiers).
	for _, childQ := range expr.GetQuantifiers() {
		childRef := childQ.GetRangesOver()
		if childRef == nil {
			continue
		}
		for _, member := range childRef.AllMembers() {
			expressionCorrelationSet(member, out)
		}
	}
}

// getExplodeExpressionFromRef returns the ExplodeExpression from a
// Reference if any of its members is one, nil otherwise.
func getExplodeExpressionFromRef(ref *expressions.Reference) *expressions.ExplodeExpression {
	for _, member := range ref.AllMembers() {
		if e, ok := member.(*expressions.ExplodeExpression); ok {
			return e
		}
	}
	return nil
}

var _ ExpressionRule = (*SplitSelectExtractIndependentQuantifiersRule)(nil)
