package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
)

// FilterDedupPredicatesRule removes structurally-duplicate predicates
// from a LogicalFilter's predicate list.
//
//	Filter([P, Q, P], X)  →  Filter([P, Q], X)
//
// SQL semantics: WHERE P AND Q AND P = WHERE P AND Q (idempotent
// AND). The duplicate adds no row-rejection power.
//
// Soundness via Explain text: two predicates compare equal iff
// their Explain() rendering matches. Same bridge as the existing
// `valueNamesEqual` helper in the predicates package.
//
// Termination: yields Filter with deduped list, REUSING the inner
// Quantifier. Pointer-identity dedup absorbs second fire.
//
// Composes with FilterMergeRule (which can leave duplicates after
// flattening nested Filters) and AndDedupRule (which dedupes inside
// an AND predicate at the predicate-tree level — different layer).
type FilterDedupPredicatesRule struct {
	matcher matching.BindingMatcher
}

// NewFilterDedupPredicatesRule constructs the rule.
func NewFilterDedupPredicatesRule() *FilterDedupPredicatesRule {
	return &FilterDedupPredicatesRule{
		matcher: NewExpressionMatcher[*expressions.LogicalFilterExpression]("logical_filter"),
	}
}

// Matcher returns the pattern.
func (r *FilterDedupPredicatesRule) Matcher() matching.BindingMatcher { return r.matcher }

// OnMatch fires when the predicate list contains at least one
// Explain-equal duplicate pair.
func (r *FilterDedupPredicatesRule) OnMatch(call *ExpressionRuleCall) {
	f := matching.Get[*expressions.LogicalFilterExpression](call.Bindings, r.matcher)
	deduped, removed := dedupFilterPredicates(f.GetPredicates())
	if !removed {
		return
	}
	call.Yield(expressions.NewLogicalFilterExpression(deduped, f.GetInner()))
}

// dedupFilterPredicates returns a slice where each predicate's
// Explain() rendering appears at most once, preserving original
// order. Returns the deduped slice + a boolean indicating whether
// any pruning happened.
func dedupFilterPredicates(preds []predicates.QueryPredicate) ([]predicates.QueryPredicate, bool) {
	seen := map[string]struct{}{}
	out := make([]predicates.QueryPredicate, 0, len(preds))
	removed := false
	for _, p := range preds {
		key := p.Explain()
		if _, dup := seen[key]; dup {
			removed = true
			continue
		}
		seen[key] = struct{}{}
		out = append(out, p)
	}
	return out, removed
}

var _ ExpressionRule = (*FilterDedupPredicatesRule)(nil)
