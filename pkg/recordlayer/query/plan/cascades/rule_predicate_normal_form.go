// Portions derived from FoundationDB Record Layer (NormalFormRule.java,
// QueryPredicateWithDnfRuleSet.java),
// Copyright 2015-2023 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

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

func (r *predicateDNFRule) rootOnly() bool { return true }

func (r *predicateDNFRule) OnMatch(call *RuleCall) {
	if !call.isRoot {
		return
	}
	pred := call.Bindings.Get(r.matcher).(predicates.QueryPredicate)
	if normalized, changed := NormalizeDNFWithoutSimplification(pred, NormalizerDefaultSizeLimit); changed {
		call.YieldAndReExplore(normalized)
	}
}
