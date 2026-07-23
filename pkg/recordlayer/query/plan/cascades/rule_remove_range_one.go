package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// RemoveRangeOneRule ports Java's RemoveRangeOneRule (ExplorationCascadesRule
// <SelectExpression>). It drops a ForEach quantifier over a RANGE(0, 1)
// table-function value from a SelectExpression when (1) the Select has MORE THAN
// ONE quantifier (removing the sole quantifier would change cardinality) and
// (2) that quantifier's alias is UNREFERENCED by the result value, the
// predicates, and every sibling quantifier. A RANGE(0, 1) is a single-row
// cross-join identity inserted to keep a Select non-empty (see
// DecorrelateValuesRule); once other real quantifiers exist and nothing reads
// it, it is dead weight.
//
// This is distinct from the Go-invented LIMIT-removal rule that wore this name
// and was deleted in RFC-188 — this is Java's ACTUAL RemoveRangeOneRule. It is
// latent today (Go only mints the RANGE(0, 1) placeholder when ALL quantifiers
// are values-boxes, yielding a single-quantifier Select that the >1 guard
// excludes), ported for parity.
type RemoveRangeOneRule struct {
	matcher matching.BindingMatcher
}

// NewRemoveRangeOneRule constructs the rule.
func NewRemoveRangeOneRule() *RemoveRangeOneRule {
	return &RemoveRangeOneRule{
		matcher: NewExpressionMatcher[*expressions.SelectExpression]("remove_range_one"),
	}
}

// Matcher returns the pattern.
func (r *RemoveRangeOneRule) Matcher() matching.BindingMatcher { return r.matcher }

// OnMatch removes an unreferenced RANGE(0, 1) quantifier.
func (r *RemoveRangeOneRule) OnMatch(call *ExpressionRuleCall) {
	sel := matching.Get[*expressions.SelectExpression](call.Bindings, r.matcher)
	quantifiers := sel.GetQuantifiers()
	// There must be at least one OTHER quantifier — removing the sole quantifier
	// changes the Select's cardinality (Java's size()==1 guard).
	if len(quantifiers) <= 1 {
		return
	}
	// Only INNER/CROSS joins: removing a one-row RANGE leg from a LEFT OUTER
	// SelectExpression changes cardinality (a preserved RANGE side with an empty
	// counterpart emits one null-extended row before but zero after). And decline
	// a quantifier-swapped Select: reconstructing it would
	// have to restore the swap + its SQL column ordering, which the removal path
	// does not model — declining is always safe for an optimization rule.
	if sel.GetJoinType() != expressions.JoinInner && sel.GetJoinType() != expressions.JoinCross {
		return
	}
	if sel.IsQuantifiersSwapped() {
		return
	}

	for i, q := range quantifiers {
		if q.Kind() != expressions.QuantifierForEach || !isRangeOneQuantifier(q) {
			continue
		}
		id := q.GetAlias()

		// The alias must be referenced by nothing: not the result value, not any
		// predicate, and not any SIBLING quantifier (Java's isCorrelatedTo checks).
		if _, ref := values.GetCorrelatedToOfValue(sel.GetResultValue())[id]; ref {
			continue
		}
		if anyPredicateCorrelatedTo(sel.GetPredicates(), id) {
			continue
		}
		if anySiblingCorrelatedTo(quantifiers, i, id) {
			continue
		}

		// A RANGE(0, 1) quantifier whose value is never referenced — drop it,
		// preserving the Select's source aliases (minus the removed leg's) and
		// join type so later NLJ implementation still qualifies rows correctly
		// (NewSelectExpression alone discards this metadata).
		remaining := make([]expressions.Quantifier, 0, len(quantifiers)-1)
		for j, other := range quantifiers {
			if j != i {
				remaining = append(remaining, other)
			}
		}
		remainingAliases := removeSourceAliasAt(sel.GetSourceAliases(), i)
		call.Yield(expressions.NewSelectExpressionWithJoinType(
			sel.GetResultValue(), remaining, sel.GetPredicates(), remainingAliases, sel.GetJoinType(),
		))
		return
	}
}

// removeSourceAliasAt drops the alias parallel to the removed quantifier at index
// i, tolerating a nil/short sourceAliases slice (aliases need not be present for
// every quantifier).
func removeSourceAliasAt(aliases []string, i int) []string {
	if i < 0 || i >= len(aliases) {
		return aliases
	}
	out := make([]string, 0, len(aliases)-1)
	out = append(out, aliases[:i]...)
	out = append(out, aliases[i+1:]...)
	return out
}

func anyPredicateCorrelatedTo(preds []predicates.QueryPredicate, id values.CorrelationIdentifier) bool {
	for _, p := range preds {
		if _, ok := p.GetCorrelatedTo()[id]; ok {
			return true
		}
	}
	return false
}

func anySiblingCorrelatedTo(quantifiers []expressions.Quantifier, self int, id values.CorrelationIdentifier) bool {
	for j, other := range quantifiers {
		if j == self {
			continue
		}
		if _, ok := other.GetCorrelatedTo()[id]; ok {
			return true
		}
	}
	return false
}

var _ ExpressionRule = (*RemoveRangeOneRule)(nil)
