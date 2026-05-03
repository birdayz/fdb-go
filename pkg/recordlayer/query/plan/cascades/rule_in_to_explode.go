package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
)

// InComparisonToExplodeRule rewrites a LogicalFilterExpression whose
// predicate list contains a ComparisonPredicate with ComparisonIn into
// a LogicalUnionExpression of filters, one per IN-list element, each
// using an equality comparison on that element.
//
//	Filter([col IN (v1, v2, v3), ...other...], inner)
//	  →  Union(
//	       Filter([col = v1, ...other...], inner),
//	       Filter([col = v2, ...other...], inner),
//	       Filter([col = v3, ...other...], inner),
//	     )
//
// This enables the index-pushdown pipeline: each equality leg can now
// independently match an index prefix, yielding N index equality scans
// that the ImplementUnionRule merges into a physical UnionPlan.
//
// Java equivalent: `InComparisonToExplodeRule` in the Cascades EXPLORE
// phase. Only fires when the IN-list is a compile-time constant (the
// Operand evaluates to a []any). Dynamic IN-lists (parameter markers,
// correlated subqueries) are left intact for the runtime IN evaluator.
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
