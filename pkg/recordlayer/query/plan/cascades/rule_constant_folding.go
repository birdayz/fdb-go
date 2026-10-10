// Portions derived from FoundationDB Record Layer (ConstantFoldingRuleSet.java,
// ValuePredicate.java, ValuePredicateSimplificationRule.java,
// ConstantFoldingValuePredicateRule.java, and others),
// Copyright 2015-2022 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2023 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2025 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// ConstantFoldingRules is Java's ConstantFoldingRuleSet
// (ConstantFoldingRuleSet.java:36-79): the default predicate rules
// (queryPredicateSimplificationRules: identity and annulment for AND and OR,
// absorption, NOT over a comparison, De Morgan), ValuePredicateSimplification-
// Rule over the PREDICATE value set, and the constant folds over EFFECTIVE
// constants -- ConstantFoldingValuePredicateRule here, and the
// PredicateWithRanges and MultiConstraint folds, which the value
// simplification of a PredicateWithValueAndRanges applies
// (predicates.SimplifyPredicateValues).
//
// It has Java's two consumers: QueryPredicateSimplificationRule over a
// select's or filter's whole conjunction, and foldPredicateAtNull, which
// decides whether a predicate rejects a null-on-empty quantifier's NULL row.
// Nothing here evaluates a composite at plan time, and NOT over a constant
// predicate does not fold: Java has neither.
func ConstantFoldingRules() []CascadesRule {
	return append(queryPredicateSimplificationRules(),
		NewValuePredicateSimplificationRule(),
		NewConstantFoldingValuePredicateRule(),
		NewConstantFoldingBooleanValuePredicateRule(),
	)
}

// ValuePredicateSimplificationRule is Java's rule of that name
// (ValuePredicateSimplificationRule.java:62-74): it simplifies a leaf
// predicate's values with the PREDICATE value set -- a comparison's operand
// and comparand, a boolean value predicate's value, and a predicate with
// ranges' value and comparands (whose constant folds run with it).
type ValuePredicateSimplificationRule struct {
	matcher matching.BindingMatcher
}

// NewValuePredicateSimplificationRule constructs the rule.
func NewValuePredicateSimplificationRule() *ValuePredicateSimplificationRule {
	return &ValuePredicateSimplificationRule{
		matcher: &predicateMatcher[predicates.QueryPredicate]{rootType: "QueryPredicate"},
	}
}

func (r *ValuePredicateSimplificationRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *ValuePredicateSimplificationRule) OnMatch(call *RuleCall) {
	p := call.Bindings.Get(r.matcher).(predicates.QueryPredicate)
	switch p.(type) {
	case *predicates.ComparisonPredicate, *predicates.ValuePredicate, *predicates.PredicateWithValueAndRanges:
	default:
		return
	}
	simplified := predicates.SimplifyPredicateValues(p)
	// The driver's fixpoint is pointer identity: an equal rebuild must not
	// count as progress.
	if simplified == p || predicates.PredicateEquals(simplified, p) {
		return
	}
	call.Yield(simplified)
}

// ConstantFoldingValuePredicateRule is Java's rule of that name
// (ConstantFoldingValuePredicateRule.java:40-88) over Go's ComparisonPredicate,
// which is Java's ValuePredicate: it folds the comparison when its operands are
// effective constants (predicates.FoldComparisonMaybe).
type ConstantFoldingValuePredicateRule struct {
	matcher matching.BindingMatcher
}

// NewConstantFoldingValuePredicateRule constructs the rule.
func NewConstantFoldingValuePredicateRule() *ConstantFoldingValuePredicateRule {
	return &ConstantFoldingValuePredicateRule{matcher: newComparisonPredicateMatcher()}
}

func (r *ConstantFoldingValuePredicateRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *ConstantFoldingValuePredicateRule) OnMatch(call *RuleCall) {
	cp := call.Bindings.Get(r.matcher).(*predicates.ComparisonPredicate)
	if folded := predicates.FoldComparisonMaybe(cp.Operand, cp.Comparison); folded != nil {
		call.Yield(folded)
	}
}

// ConstantFoldingBooleanValuePredicateRule is ConstantFoldingValuePredicateRule
// for Go's ValuePredicate, a boolean value used as a predicate. Java builds
// that as ValuePredicate(value, EQUALS TRUE) (Expression.Utils.toUnderlying-
// Predicate), so it folds as that comparison: TRUE, FALSE or NULL over an
// effective constant, nothing otherwise.
type ConstantFoldingBooleanValuePredicateRule struct {
	matcher matching.BindingMatcher
}

// NewConstantFoldingBooleanValuePredicateRule constructs the rule.
func NewConstantFoldingBooleanValuePredicateRule() *ConstantFoldingBooleanValuePredicateRule {
	return &ConstantFoldingBooleanValuePredicateRule{matcher: newValuePredicateMatcher()}
}

func (r *ConstantFoldingBooleanValuePredicateRule) Matcher() matching.BindingMatcher {
	return r.matcher
}

func (r *ConstantFoldingBooleanValuePredicateRule) OnMatch(call *RuleCall) {
	vp := call.Bindings.Get(r.matcher).(*predicates.ValuePredicate)
	if vp.Value == nil {
		return
	}
	isTrue := predicates.Comparison{Type: predicates.ComparisonEquals, Operand: values.NewBooleanValue(true)}
	if folded := predicates.FoldComparisonMaybe(vp.Value, isTrue); folded != nil {
		call.Yield(folded)
	}
}

func newComparisonPredicateMatcher() *predicateMatcher[*predicates.ComparisonPredicate] {
	return &predicateMatcher[*predicates.ComparisonPredicate]{rootType: "ComparisonPredicate"}
}

func newValuePredicateMatcher() *predicateMatcher[*predicates.ValuePredicate] {
	return &predicateMatcher[*predicates.ValuePredicate]{rootType: "ValuePredicate"}
}

var (
	_ CascadesRule = (*ValuePredicateSimplificationRule)(nil)
	_ CascadesRule = (*ConstantFoldingValuePredicateRule)(nil)
	_ CascadesRule = (*ConstantFoldingBooleanValuePredicateRule)(nil)
)
