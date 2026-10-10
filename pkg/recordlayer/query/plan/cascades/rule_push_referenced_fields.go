// Portions derived from FoundationDB Record Layer (
// PushReferencedFieldsThroughFilterRule.java,
// PushReferencedFieldsThroughSelectRule.java,
// PushReferencedFieldsThroughDistinctRule.java,
// PushReferencedFieldsThroughUniqueRule.java),
// Copyright 2015-2019 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// PushReferencedFieldsThroughFilterRule pushes referenced-field
// constraints through LogicalFilterExpression. Extracts FieldValues
// from the filter's predicates, unions them with any incoming
// constraint, and pushes the result to the child Reference.
//
// Ports Java's PushReferencedFieldsThroughFilterRule.
type PushReferencedFieldsThroughFilterRule struct {
	preOrderMarker
	matcher matching.BindingMatcher
}

func NewPushReferencedFieldsThroughFilterRule() *PushReferencedFieldsThroughFilterRule {
	return &PushReferencedFieldsThroughFilterRule{
		matcher: NewExpressionMatcher[*expressions.LogicalFilterExpression]("push_ref_fields_filter"),
	}
}

func (r *PushReferencedFieldsThroughFilterRule) Matcher() matching.BindingMatcher { return r.matcher }

// ConstraintDependencies is Java's ImmutableSet.of(REFERENCED_FIELDS).
func (r *PushReferencedFieldsThroughFilterRule) ConstraintDependencies() []any {
	return []any{ReferencedFieldsConstraintKey}
}

func (r *PushReferencedFieldsThroughFilterRule) OnMatch(call *ImplementationRuleCall) {
	if !call.IsConstraintOnly() {
		return
	}
	filter := call.Bindings.Get(r.matcher).(*expressions.LogicalFilterExpression)

	fromPreds := extractFieldsFromPredicates(filter)
	existing, _ := Get(call.Constraints, call.Reference, ReferencedFieldsConstraintKey)
	merged := fromPreds.Union(existing)

	childRef := filter.GetInner().GetRangesOver()
	if childRef != nil {
		Set(call.Constraints, childRef, ReferencedFieldsConstraintKey, merged)
	}
}

// PushReferencedFieldsThroughSelectRule pushes referenced-field
// constraints through SelectExpression. Extracts FieldValues from
// predicates and the result value, unions with incoming constraint,
// and pushes to all child References.
//
// Ports Java's PushReferencedFieldsThroughSelectRule.
type PushReferencedFieldsThroughSelectRule struct {
	preOrderMarker
	matcher matching.BindingMatcher
}

func NewPushReferencedFieldsThroughSelectRule() *PushReferencedFieldsThroughSelectRule {
	return &PushReferencedFieldsThroughSelectRule{
		matcher: NewExpressionMatcher[*expressions.SelectExpression]("push_ref_fields_select"),
	}
}

func (r *PushReferencedFieldsThroughSelectRule) Matcher() matching.BindingMatcher { return r.matcher }

// ConstraintDependencies is Java's ImmutableSet.of(REFERENCED_FIELDS).
func (r *PushReferencedFieldsThroughSelectRule) ConstraintDependencies() []any {
	return []any{ReferencedFieldsConstraintKey}
}

func (r *PushReferencedFieldsThroughSelectRule) OnMatch(call *ImplementationRuleCall) {
	if !call.IsConstraintOnly() {
		return
	}
	sel := call.Bindings.Get(r.matcher).(*expressions.SelectExpression)

	fromPreds := extractFieldsFromPredicates(sel)
	fromResult := FieldValuesFromValue(sel.GetResultValue())
	existing, _ := Get(call.Constraints, call.Reference, ReferencedFieldsConstraintKey)
	merged := fromPreds.Union(fromResult).Union(existing)

	for _, q := range sel.GetQuantifiers() {
		if childRef := q.GetRangesOver(); childRef != nil {
			Set(call.Constraints, childRef, ReferencedFieldsConstraintKey, merged)
		}
	}
}

// PushReferencedFieldsThroughDistinctRule pushes referenced-field
// constraints through LogicalDistinctExpression (transparent pass-through).
//
// Ports Java's PushReferencedFieldsThroughDistinctRule.
type PushReferencedFieldsThroughDistinctRule struct {
	preOrderMarker
	matcher matching.BindingMatcher
}

func NewPushReferencedFieldsThroughDistinctRule() *PushReferencedFieldsThroughDistinctRule {
	return &PushReferencedFieldsThroughDistinctRule{
		matcher: NewExpressionMatcher[*expressions.LogicalDistinctExpression]("push_ref_fields_distinct"),
	}
}

func (r *PushReferencedFieldsThroughDistinctRule) Matcher() matching.BindingMatcher {
	return r.matcher
}

func (r *PushReferencedFieldsThroughDistinctRule) hasConstraintEffect(cm *ConstraintMap, ref *expressions.Reference, expr expressions.RelationalExpression) bool {
	return passThroughConstraintHasEffect(cm, ref, expr, ReferencedFieldsConstraintKey)
}

// ConstraintDependencies is Java's ImmutableSet.of(REFERENCED_FIELDS).
func (r *PushReferencedFieldsThroughDistinctRule) ConstraintDependencies() []any {
	return []any{ReferencedFieldsConstraintKey}
}

func (r *PushReferencedFieldsThroughDistinctRule) OnMatch(call *ImplementationRuleCall) {
	if !call.IsConstraintOnly() {
		return
	}
	d := call.Bindings.Get(r.matcher).(*expressions.LogicalDistinctExpression)
	existing, ok := Get(call.Constraints, call.Reference, ReferencedFieldsConstraintKey)
	if !ok {
		return
	}

	qs := d.GetQuantifiers()
	if len(qs) > 0 {
		if childRef := qs[0].GetRangesOver(); childRef != nil {
			Set(call.Constraints, childRef, ReferencedFieldsConstraintKey, existing)
		}
	}
}

// PushReferencedFieldsThroughUniqueRule pushes referenced-field
// constraints through LogicalUniqueExpression (transparent pass-through).
//
// Ports Java's PushReferencedFieldsThroughUniqueRule.
type PushReferencedFieldsThroughUniqueRule struct {
	preOrderMarker
	matcher matching.BindingMatcher
}

func NewPushReferencedFieldsThroughUniqueRule() *PushReferencedFieldsThroughUniqueRule {
	return &PushReferencedFieldsThroughUniqueRule{
		matcher: NewExpressionMatcher[*expressions.LogicalUniqueExpression]("push_ref_fields_unique"),
	}
}

func (r *PushReferencedFieldsThroughUniqueRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *PushReferencedFieldsThroughUniqueRule) hasConstraintEffect(cm *ConstraintMap, ref *expressions.Reference, expr expressions.RelationalExpression) bool {
	return passThroughConstraintHasEffect(cm, ref, expr, ReferencedFieldsConstraintKey)
}

// ConstraintDependencies is Java's ImmutableSet.of(REFERENCED_FIELDS).
func (r *PushReferencedFieldsThroughUniqueRule) ConstraintDependencies() []any {
	return []any{ReferencedFieldsConstraintKey}
}

func (r *PushReferencedFieldsThroughUniqueRule) OnMatch(call *ImplementationRuleCall) {
	if !call.IsConstraintOnly() {
		return
	}
	u := call.Bindings.Get(r.matcher).(*expressions.LogicalUniqueExpression)
	existing, ok := Get(call.Constraints, call.Reference, ReferencedFieldsConstraintKey)
	if !ok {
		return
	}

	qs := u.GetQuantifiers()
	if len(qs) > 0 {
		if childRef := qs[0].GetRangesOver(); childRef != nil {
			Set(call.Constraints, childRef, ReferencedFieldsConstraintKey, existing)
		}
	}
}

// extractFieldsFromPredicates extracts FieldValue names from an
// expression's predicates.
func extractFieldsFromPredicates(e expressions.RelationalExpressionWithPredicates) *ReferencedFields {
	fields := map[string]struct{}{}
	for _, p := range e.GetPredicates() {
		collectPredicateFieldValues(p, fields)
	}
	return NewReferencedFields(fields)
}

func collectPredicateFieldValues(p predicates.QueryPredicate, out map[string]struct{}) {
	predicates.ReplaceValues(p, func(v values.Value) values.Value {
		if _, ok := values.AsFieldValue(v); ok {
			collectFieldNamesFromValue(v, out)
		}
		return v
	})
}

var (
	_ ImplementationRule = (*PushReferencedFieldsThroughFilterRule)(nil)
	_ ImplementationRule = (*PushReferencedFieldsThroughSelectRule)(nil)
	_ ImplementationRule = (*PushReferencedFieldsThroughDistinctRule)(nil)
	_ ImplementationRule = (*PushReferencedFieldsThroughUniqueRule)(nil)
)
