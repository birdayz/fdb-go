package cascades

import (
	"strings"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// PushOrderingThroughProjectionRule pushes a LogicalSort's ordering
// through a LogicalProjection when the sort keys can be expressed
// in terms of the pre-projection columns.
//
//	Sort([expr1 ASC], Projection([expr1=A+B, expr2=C], X))
//	  → Projection([expr1=A+B, expr2=C], Sort([A+B ASC], X))
//
// Uses RequestedOrdering.PushDownThroughValue to translate sort keys
// through the projection's result value.
//
// The projection's result value is modelled as a RecordConstructorValue
// mapping output alias names to their projected expressions. Each sort
// key (which references post-projection columns by alias) is resolved
// to the corresponding pre-projection expression via PushDownThroughValue.
// If all sort keys translate, the sort is pushed below the projection.
//
// Soundness: projection is a pure column-rename/compute — it does not
// filter, reorder, or duplicate rows. Moving the sort below is
// equivalent to sorting first, then projecting — row order is
// preserved through the projection.
//
// Ports Java's PushRequestedOrderingThroughSelectRule (for the
// projection case). Java uses a constraint-push model; Go uses a
// structural rewrite that achieves the same effect.
type PushOrderingThroughProjectionRule struct {
	matcher matching.BindingMatcher
}

func NewPushOrderingThroughProjectionRule() *PushOrderingThroughProjectionRule {
	return &PushOrderingThroughProjectionRule{
		matcher: NewExpressionMatcher[*expressions.LogicalSortExpression]("push_ordering_through_projection"),
	}
}

func (r *PushOrderingThroughProjectionRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *PushOrderingThroughProjectionRule) OnMatch(call *ExpressionRuleCall) {
	s := matching.Get[*expressions.LogicalSortExpression](call.Bindings, r.matcher)
	if s.IsUnsorted() {
		return
	}

	innerExpr := s.GetInner().GetRangesOver().Get()
	proj, ok := innerExpr.(*expressions.LogicalProjectionExpression)
	if !ok {
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

	// Convert sort keys to a RequestedOrdering so we can use
	// PushDownThroughValue to translate through the result value.
	requestedOrdering := sortExpressionToRequestedOrdering(s)

	// The alias here is the quantifier alias between the sort and
	// the projection — the sort's inner quantifier.
	alias := s.GetInner().GetAlias()

	pushed := requestedOrdering.PushDownThroughValue(resultValue, alias)

	// PushDownThroughValue drops parts it cannot translate. If any
	// were dropped (or the result is a preserve ordering with no
	// parts), the push failed.
	if pushed.IsPreserve() || pushed.Size() != requestedOrdering.Size() {
		return
	}

	// Convert the pushed RequestedOrdering back to SortKeys for the
	// new LogicalSortExpression.
	pushedParts := pushed.GetParts()
	newSortKeys := make([]expressions.SortKey, len(pushedParts))
	for i, p := range pushedParts {
		newSortKeys[i] = expressions.SortKey{
			Value:   p.Value,
			Reverse: p.SortOrder == RequestedSortOrderDescending,
		}
	}

	// Build: Projection(values, Sort(translatedKeys, projChild))
	pushedSort := expressions.NewLogicalSortExpression(newSortKeys, proj.GetInner())
	pushedSortQ := expressions.ForEachQuantifier(call.MemoizeExpression(pushedSort))
	var newProj *expressions.LogicalProjectionExpression
	if len(aliases) > 0 {
		newProj = expressions.NewLogicalProjectionExpressionWithAliases(projValues, aliases, pushedSortQ)
	} else {
		newProj = expressions.NewLogicalProjectionExpression(projValues, pushedSortQ)
	}
	call.Yield(newProj)
}

var _ ExpressionRule = (*PushOrderingThroughProjectionRule)(nil)
