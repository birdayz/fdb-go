package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// PushFilterThroughDistinctRule pushes a LogicalFilter under a
// LogicalDistinct.
//
//	Filter(P, Distinct(X))  →  Distinct(Filter(P, X))
//
// Soundness: the inner Distinct doesn't reshape rows (its
// GetResultValue is the inner's flowed object value), so FieldValue
// references inside P resolve against the same row shape on both
// sides of the rewrite. Output row sets are equal because both forms
// yield "DISTINCT rows of X that satisfy P".
//
// Optimization argument: filtering BEFORE dedup is faster — the
// dedup runs over fewer rows. Eliminated rows can never become
// duplicates after-the-fact.
//
// Termination: the rule yields a Distinct wrapping a Quantifier
// over a fresh Reference holding the new Filter. The fresh Reference
// would defeat the sameChildReferences pointer-identity dedup, but
// Reference.Insert's SemanticEquals fallback (post-680e664a) catches
// the structural equivalence and absorbs the second yield.
//
// Java equivalent: emerges from cost preference for cheaper plans.
type PushFilterThroughDistinctRule struct {
	matcher matching.BindingMatcher
}

// NewPushFilterThroughDistinctRule constructs the rule.
func NewPushFilterThroughDistinctRule() *PushFilterThroughDistinctRule {
	return &PushFilterThroughDistinctRule{
		matcher: NewExpressionMatcher[*expressions.LogicalFilterExpression]("logical_filter"),
	}
}

// Matcher returns the pattern.
func (r *PushFilterThroughDistinctRule) Matcher() matching.BindingMatcher { return r.matcher }

// OnMatch fires when the inner Quantifier ranges over a
// LogicalDistinctExpression.
func (r *PushFilterThroughDistinctRule) OnMatch(call *ExpressionRuleCall) {
	f := matching.Get[*expressions.LogicalFilterExpression](call.Bindings, r.matcher)
	innerExpr := f.GetInner().GetRangesOver().Get()
	dist, ok := innerExpr.(*expressions.LogicalDistinctExpression)
	if !ok {
		return
	}
	// The filter predicate is rooted at the quantifier over Distinct.  Once it
	// moves below Distinct it must read Distinct's input quantifier instead;
	// reusing the old alias leaves an unbound QOV in the rewritten expression.
	rebasedPredicates, err := rebasePushFilterPredicates(
		f.GetPredicates(), f.GetInner().GetAlias(), dist.GetInner().GetAlias())
	if err != nil {
		call.Fail(err)
		return
	}
	// Build Filter(P, dist.inner-source) — REUSE dist's inner
	// Quantifier so the pushed Filter shares the same Reference
	// pointer as the input.
	pushed, err := expressions.NewLogicalFilterExpression(rebasedPredicates, dist.GetInner())
	if err != nil {
		call.Fail(err)
		return
	}
	pushedQ := expressions.ForEachQuantifier(call.MemoizeExpression(pushed))
	outer, err := expressions.NewLogicalDistinctExpression(pushedQ)
	if err != nil {
		call.Fail(err)
		return
	}
	call.Yield(outer)
}

// rebasePushFilterPredicates is the checked alias-transition authority shared
// by the logical filter-pushdown rules.  Predicate rebuilding is atomic: an
// incompatible exact FieldValue root returns an error instead of publishing a
// nil or partially rebased predicate tree.
func rebasePushFilterPredicates(
	input []predicates.QueryPredicate,
	source, target values.CorrelationIdentifier,
) ([]predicates.QueryPredicate, error) {
	if source == target {
		return append([]predicates.QueryPredicate(nil), input...), nil
	}
	aliases, err := values.NewAliasMap([]values.AliasPair{{Source: source, Target: target}})
	if err != nil {
		return nil, err
	}
	result := make([]predicates.QueryPredicate, len(input))
	for i, predicate := range input {
		result[i], err = predicates.TransformEmbeddedValuesChecked(
			predicate,
			func(value values.Value) (values.Value, error) {
				return values.RebaseValueChecked(value, aliases)
			},
		)
		if err != nil {
			return nil, err
		}
	}
	return result, nil
}

var _ ExpressionRule = (*PushFilterThroughDistinctRule)(nil)
