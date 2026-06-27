package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
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

func (r *PushReferencedFieldsThroughDistinctRule) OnMatch(call *ImplementationRuleCall) {
	if !call.IsConstraintOnly() {
		return
	}
	d := call.Bindings.Get(r.matcher).(*expressions.LogicalDistinctExpression)
	existing, _ := Get(call.Constraints, call.Reference, ReferencedFieldsConstraintKey)

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

func (r *PushReferencedFieldsThroughUniqueRule) OnMatch(call *ImplementationRuleCall) {
	if !call.IsConstraintOnly() {
		return
	}
	u := call.Bindings.Get(r.matcher).(*expressions.LogicalUniqueExpression)
	existing, _ := Get(call.Constraints, call.Reference, ReferencedFieldsConstraintKey)

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

func collectPredicateFieldValues(p any, out map[string]struct{}) {
	if p == nil {
		return
	}
	switch pred := p.(type) {
	case *predicates.ComparisonPredicate:
		collectFieldNamesFromValue(pred.Operand, out)
		collectFieldNamesFromValue(pred.Comparison.Operand, out)
	case *predicates.ValuePredicate:
		collectFieldNamesFromValue(pred.Value, out)
	case *predicates.AndPredicate:
		for _, sub := range pred.SubPredicates {
			collectPredicateFieldValues(sub, out)
		}
	case *predicates.OrPredicate:
		for _, sub := range pred.SubPredicates {
			collectPredicateFieldValues(sub, out)
		}
	case *predicates.NotPredicate:
		collectPredicateFieldValues(pred.Child, out)
	}
}

var (
	_ ImplementationRule = (*PushReferencedFieldsThroughFilterRule)(nil)
	_ ImplementationRule = (*PushReferencedFieldsThroughSelectRule)(nil)
	_ ImplementationRule = (*PushReferencedFieldsThroughDistinctRule)(nil)
	_ ImplementationRule = (*PushReferencedFieldsThroughUniqueRule)(nil)
)
