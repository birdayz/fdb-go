package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
)

// predicateDNFRule is Java's root-only NormalFormRule for QueryPredicateWithDnfRuleSet.
type predicateDNFRule struct {
	matcher matching.BindingMatcher
}

func newPredicateDNFRule() *predicateDNFRule {
	return &predicateDNFRule{
		matcher: &predicateMatcher[predicates.QueryPredicate]{rootType: "predicate"},
	}
}

func (r *predicateDNFRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *predicateDNFRule) OnMatch(call *RuleCall) {
	if !call.isRoot {
		return
	}
	pred := call.Bindings.Get(r.matcher).(predicates.QueryPredicate)
	if normalized, changed := NormalizeDNFWithoutSimplification(pred, NormalizerDefaultSizeLimit); changed {
		call.YieldAndReExplore(normalized)
	}
}
