package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
)

// InComparisonToExplodeRule rewrites a LogicalFilterExpression whose
// predicate list contains a ComparisonPredicate with ComparisonIn.
//
// Single-element IN → simple equality (no union):
//
//	Filter([col IN (v1), ...other...], inner)
//	  →  Filter([col = v1, ...other...], inner)
//
// Multi-element IN → union of equality filters:
//
//	Filter([col IN (v1, v2, v3), ...other...], inner)
//	  →  Union(
//	       Filter([col = v1, ...other...], inner),
//	       Filter([col = v2, ...other...], inner),
//	       Filter([col = v3, ...other...], inner),
//	     )
//
// NOTE: Java's InComparisonToExplodeRule creates a SelectExpression
// with a ForEach quantifier over ExplodeExpression, then
// AbstractDataAccessRule resolves predicates against index candidates.
// Go doesn't yet have AbstractDataAccessRule for SelectExpression
// predicates, so multi-element IN uses the Union approach which allows
// each filter leg to independently match indexes. The Java
// architecture should be ported as a follow-up.
//
// Guards:
//   - At least one ComparisonIn predicate.
//   - The IN-list Operand must evaluate (without row context) to a
//     non-empty []any.
//   - The filter must have an inner Quantifier (no bare filter).
type InComparisonToExplodeRule struct {
	matcher matching.BindingMatcher
}

func NewInComparisonToExplodeRule() *InComparisonToExplodeRule {
	return &InComparisonToExplodeRule{
		matcher: NewExpressionMatcher[*expressions.LogicalFilterExpression]("logical_filter_in_explode"),
	}
}

func (r *InComparisonToExplodeRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *InComparisonToExplodeRule) OnMatch(call *ExpressionRuleCall) {
	f := matching.Get[*expressions.LogicalFilterExpression](call.Bindings, r.matcher)
	preds := f.GetPredicates()

	inIdx := -1
	var inPred *predicates.ComparisonPredicate
	for i, p := range preds {
		cp, ok := p.(*predicates.ComparisonPredicate)
		if !ok {
			continue
		}
		if cp.Comparison.Type == predicates.ComparisonIn {
			inIdx = i
			inPred = cp
			break
		}
	}
	if inIdx < 0 {
		return
	}

	rhs := inPred.Comparison.Operand.Evaluate(nil)
	list, ok := rhs.([]any)
	if !ok || len(list) == 0 {
		return
	}

	innerRef := f.GetInner().GetRangesOver()
	if innerRef == nil {
		return
	}

	otherPreds := make([]predicates.QueryPredicate, 0, len(preds)-1)
	for i, p := range preds {
		if i != inIdx {
			otherPreds = append(otherPreds, p)
		}
	}

	// Single-element IN → simple equality.
	if len(list) == 1 {
		eqCmp := predicates.NewLiteralComparison(predicates.ComparisonEquals, list[0])
		eqPred := predicates.NewComparisonPredicate(inPred.Operand, eqCmp)
		newPreds := make([]predicates.QueryPredicate, 0, len(otherPreds)+1)
		newPreds = append(newPreds, eqPred)
		newPreds = append(newPreds, otherPreds...)
		innerQ := expressions.ForEachQuantifier(call.MemoizeExpression(innerRef.Get()))
		call.Yield(expressions.NewLogicalFilterExpression(newPreds, innerQ))
		return
	}

	// Multi-element IN → union of equality filters.
	legs := make([]expressions.Quantifier, 0, len(list))
	for _, elem := range list {
		eqCmp := predicates.NewLiteralComparison(predicates.ComparisonEquals, elem)
		eqPred := predicates.NewComparisonPredicate(inPred.Operand, eqCmp)

		legPreds := make([]predicates.QueryPredicate, 0, len(otherPreds)+1)
		legPreds = append(legPreds, eqPred)
		legPreds = append(legPreds, otherPreds...)

		innerQ := expressions.ForEachQuantifier(call.MemoizeExpression(innerRef.Get()))
		legFilter := expressions.NewLogicalFilterExpression(legPreds, innerQ)
		legRef := call.MemoizeExpression(legFilter)
		legs = append(legs, expressions.ForEachQuantifier(legRef))
	}

	union := expressions.NewLogicalUnionExpression(legs)
	call.Yield(union)
}

var _ ExpressionRule = (*InComparisonToExplodeRule)(nil)
