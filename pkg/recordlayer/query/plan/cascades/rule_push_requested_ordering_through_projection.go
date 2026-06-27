package cascades

import (
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// PushRequestedOrderingThroughProjectionRule is a PLANNING-phase
// ImplementationRule that translates a RequestedOrdering constraint
// through a LogicalProjectionExpression's value mapping and pushes the
// translated ordering to the child Reference.
//
// Projection is NOT ordering-transparent — sort keys reference
// post-projection column aliases which must be translated to their
// pre-projection source expressions via PushDownThroughValue. If all
// keys translate, the translated ordering is pushed to the child; if
// any key fails to translate, nothing is pushed.
//
// This rule fires during the top-down constraint-propagation pass
// (constraintOnly=true). During the bottom-up implementation pass
// (constraintOnly=false) it is a no-op.
//
// Ports Java's PushRequestedOrderingThroughSelectRule (for the
// projection case).
type PushRequestedOrderingThroughProjectionRule struct {
	preOrderMarker
	matcher matching.BindingMatcher
}

func NewPushRequestedOrderingThroughProjectionRule() *PushRequestedOrderingThroughProjectionRule {
	return &PushRequestedOrderingThroughProjectionRule{
		matcher: &projectionPushMatcher{},
	}
}

func (r *PushRequestedOrderingThroughProjectionRule) Matcher() matching.BindingMatcher {
	return r.matcher
}

func (r *PushRequestedOrderingThroughProjectionRule) OnMatch(call *ImplementationRuleCall) {
	if !call.IsConstraintOnly() {
		return
	}

	orderings := call.GetRequestedOrderings()
	if len(orderings) == 0 {
		return
	}

	proj := call.Bindings.Get(r.matcher).(*expressions.LogicalProjectionExpression)

	innerRef := proj.GetInner().GetRangesOver()
	if innerRef == nil {
		return
	}

	// Build the projection's result value as a RecordConstructorValue.
	// Each field maps an output alias to its projected expression.
	projValues := proj.GetProjectedValues()
	aliases := proj.GetAliases()
	fields := make([]values.RecordConstructorField, len(projValues))
	for i, v := range projValues {
		name := ""
		if i < len(aliases) {
			name = aliases[i]
		}
		if name == "" {
			name = values.ExplainValue(v)
		}
		fields[i] = values.RecordConstructorField{
			Name:  strings.ToUpper(name),
			Value: v,
		}
	}
	resultValue := values.NewRecordConstructorValue(fields...)

	// The alias is the quantifier alias between the projection and its
	// parent — the projection's inner quantifier.
	alias := proj.GetInner().GetAlias()

	// Translate each ordering through the result value.
	var translated []*RequestedOrdering
	for _, reqOrd := range orderings {
		pushed := reqOrd.PushDownThroughValue(resultValue, alias)
		// PushDownThroughValue drops parts it cannot translate. If any
		// were dropped (or the result is a preserve ordering with no
		// parts), the push failed for this ordering.
		if !pushed.IsPreserve() && pushed.Size() == reqOrd.Size() {
			translated = append(translated, pushed)
		}
	}

	if len(translated) > 0 {
		call.PushConstraint(innerRef, translated)
	}
}

var _ ImplementationRule = (*PushRequestedOrderingThroughProjectionRule)(nil)

// projectionPushMatcher matches LogicalProjectionExpression for the
// constraint-push rule.
type projectionPushMatcher struct{}

func (m *projectionPushMatcher) RootType() string { return "LogicalProjectionExpression" }

func (m *projectionPushMatcher) BindMatches(outer *matching.PlannerBindings, in any) []*matching.PlannerBindings {
	if _, ok := in.(*expressions.LogicalProjectionExpression); !ok {
		return nil
	}
	return []*matching.PlannerBindings{outer.Bind(m, in)}
}
