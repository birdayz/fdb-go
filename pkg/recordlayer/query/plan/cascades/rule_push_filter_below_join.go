package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// PushFilterBelowJoinRule pushes filter predicates below a join
// (SelectExpression with 2 ForEach quantifiers) when a predicate
// references columns from only one side of the join.
//
//	Filter([a.name='foo', a.id=b.aid], Select(rv, [qA, qB], [jpreds]))
//	  → Filter([a.id=b.aid], Select(rv, [qA', qB], [jpreds]))
//	    where qA' = ForEach(Filter([a.name='foo'], A))
//
// A predicate is pushable to side i when its complete correlation set
// references that owned quantifier and not its sibling. External correlations
// do not prevent pushdown, as in Java's PredicatePushDownRule. Predicates
// referencing both sides or neither owned side stay above this two-leg join.
// Source alias labels must agree with the owned quantifiers' labels, but
// dependency classification uses the quantifier identities, never those labels.
//
// The rule only fires on INNER joins (JoinInner). LEFT OUTER and
// CROSS joins have different NULL-preservation semantics that make
// predicate pushdown unsound without additional analysis.
//
// Soundness: for inner joins, Filter(P, Join(A, B)) and
// Join(Filter(P, A), B) produce the same result set when P only
// references A — the filter's admittance decision is independent of
// B's rows.
//
// Optimization argument: filtering before the join reduces the
// number of rows the join processes, which is typically O(n*m)
// becoming O(filtered_n * m).
type PushFilterBelowJoinRule struct {
	matcher matching.BindingMatcher
}

// NewPushFilterBelowJoinRule constructs the rule.
func NewPushFilterBelowJoinRule() *PushFilterBelowJoinRule {
	return &PushFilterBelowJoinRule{
		matcher: NewExpressionMatcher[*expressions.LogicalFilterExpression]("logical_filter"),
	}
}

// Matcher returns the pattern.
func (r *PushFilterBelowJoinRule) Matcher() matching.BindingMatcher { return r.matcher }

// OnMatch fires when the pattern matches a LogicalFilterExpression.
func (r *PushFilterBelowJoinRule) OnMatch(call *ExpressionRuleCall) {
	f := matching.Get[*expressions.LogicalFilterExpression](call.Bindings, r.matcher)
	innerExpr := f.GetInner().GetRangesOver().Get()
	sel, ok := innerExpr.(*expressions.SelectExpression)
	if !ok {
		return
	}

	// Only fire on inner joins with exactly 2 ForEach quantifiers.
	if sel.GetJoinType() != expressions.JoinInner {
		return
	}
	quantifiers := sel.GetQuantifiers()
	if len(quantifiers) != 2 {
		return
	}
	if quantifiers[0].Kind() != expressions.QuantifierForEach ||
		quantifiers[1].Kind() != expressions.QuantifierForEach {
		return
	}
	// Wrapping either leg below the filter reconstructs it as plain ForEach.
	// A StrictSingle edge may only move through a rewrite with an explicit
	// cardinality-preservation proof; this rule has none.
	if hasStrictSingleQuantifier(quantifiers) {
		return
	}

	aliases := sel.GetSourceAliases()
	if len(aliases) != len(quantifiers) {
		return
	}
	for i, q := range quantifiers {
		// Filtering before null extension is not equivalent to filtering the
		// null-extended row. Check the parallel source labels without turning
		// them into identities: a unique-kind alias cannot be reconstructed
		// with NamedCorrelationIdentifier, even when its label agrees.
		if q.IsNullOnEmpty() || q.GetAlias().IsZero() || q.GetAlias().Name() != aliases[i] {
			return
		}
	}

	filterPreds := f.GetPredicates()
	if len(filterPreds) == 0 {
		return
	}

	// Partition predicates into: push-to-0, push-to-1, keep-on-join.
	var pushTo0, pushTo1, keep []predicates.QueryPredicate
	for _, pred := range filterPreds {
		side := predicateSingleSide(pred, quantifiers[0].GetAlias(), quantifiers[1].GetAlias())
		switch side {
		case 0:
			pushTo0 = append(pushTo0, pred)
		case 1:
			pushTo1 = append(pushTo1, pred)
		default:
			keep = append(keep, pred)
		}
	}

	// Nothing to push — rule doesn't fire.
	if len(pushTo0) == 0 && len(pushTo1) == 0 {
		return
	}

	// Reuse the original edge inside each filter and retain its owned alias
	// on the replacement edge, as Java's PredicatePushDownRule does. The
	// Select's result value and join predicates still read those aliases.
	newQ0 := quantifiers[0]
	if len(pushTo0) > 0 {
		pushed0, err := expressions.NewLogicalFilterExpression(pushTo0, quantifiers[0])
		if err != nil {
			call.Fail(err)
			return
		}
		newQ0 = expressions.NamedForEachQuantifier(quantifiers[0].GetAlias(), call.MemoizeExpression(pushed0))
	}

	newQ1 := quantifiers[1]
	if len(pushTo1) > 0 {
		pushed1, err := expressions.NewLogicalFilterExpression(pushTo1, quantifiers[1])
		if err != nil {
			call.Fail(err)
			return
		}
		newQ1 = expressions.NamedForEachQuantifier(quantifiers[1].GetAlias(), call.MemoizeExpression(pushed1))
	}

	// Build the new SelectExpression with the modified quantifiers.
	newSel, err := expressions.NewSelectExpressionWithJoinType(
		sel.GetResultValue(),
		[]expressions.Quantifier{newQ0, newQ1},
		sel.GetPredicates(),
		aliases,
		sel.GetJoinType(),
	)
	if err != nil {
		call.Fail(err)
		return
	}

	// If all filter predicates were pushed, the filter wrapper is
	// unnecessary — yield the Select directly. Otherwise wrap in a
	// filter with the remaining predicates.
	if len(keep) == 0 {
		call.Yield(newSel)
	} else {
		selQ := expressions.ForEachQuantifier(call.MemoizeExpression(newSel))
		remaining, err := expressions.NewLogicalFilterExpression(keep, selQ)
		if err != nil {
			call.Fail(err)
			return
		}
		call.Yield(remaining)
	}
}

// predicateSingleSide returns which side of a 2-way join a predicate
// references:
//   - 0 if it only references alias0
//   - 1 if it only references alias1
//   - -1 if it references both or neither owned alias
func predicateSingleSide(pred predicates.QueryPredicate, corr0, corr1 values.CorrelationIdentifier) int {
	// A missing dependency is permission to move a predicate. Ask the
	// predicate for its transitive set rather than enumerating field depths
	// or predicate kinds, which can silently hide an owned sibling.
	correlated := predicates.GetCorrelatedToOfPredicate(pred)
	_, refs0 := correlated[corr0]
	_, refs1 := correlated[corr1]
	if refs0 && !refs1 {
		return 0
	}
	if refs1 && !refs0 {
		return 1
	}
	return -1
}

var _ ExpressionRule = (*PushFilterBelowJoinRule)(nil)
