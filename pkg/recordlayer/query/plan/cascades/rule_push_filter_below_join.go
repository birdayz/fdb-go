package cascades

import (
	"strings"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// PushFilterBelowJoinRule pushes filter predicates below a join
// (SelectExpression with 2 ForEach quantifiers) when a predicate
// references columns from only one side of the join.
//
//	Filter([a.name='foo', a.id=b.aid], Select(rv, [qA, qB], [jpreds]))
//	  → Filter([a.id=b.aid], Select(rv, [qA', qB], [jpreds]))
//	    where qA' = ForEach(Filter([a.name='foo'], A))
//
// A predicate is pushable to side i when every FieldValue in its
// tree is qualified with sourceAliases[i] and does not reference
// sourceAliases[j] (j != i). Predicates that reference both sides,
// or that have no FieldValue references at all, are kept on the
// filter above the join (conservative).
//
// The rule only fires on INNER joins (JoinInner). LEFT OUTER and
// CROSS joins have different NULL-preservation semantics that make
// predicate pushdown unsound without additional analysis.
//
// Soundness: for inner joins, Filter(P, Join(A, B)) and
// Join(Filter(P, A), B) produce the same result set when P only
// references A — the filter's admittance decision is independent of
// B's rows.
//
// Optimization argument: filtering before the join reduces the
// number of rows the join processes, which is typically O(n*m)
// becoming O(filtered_n * m).
type PushFilterBelowJoinRule struct {
	matcher matching.BindingMatcher
}

// NewPushFilterBelowJoinRule constructs the rule.
func NewPushFilterBelowJoinRule() *PushFilterBelowJoinRule {
	return &PushFilterBelowJoinRule{
		matcher: NewExpressionMatcher[*expressions.LogicalFilterExpression]("logical_filter"),
	}
}

// Matcher returns the pattern.
func (r *PushFilterBelowJoinRule) Matcher() matching.BindingMatcher { return r.matcher }

// OnMatch fires when the pattern matches a LogicalFilterExpression.
func (r *PushFilterBelowJoinRule) OnMatch(call *ExpressionRuleCall) {
	f := matching.Get[*expressions.LogicalFilterExpression](call.Bindings, r.matcher)
	innerExpr := f.GetInner().GetRangesOver().Get()
	sel, ok := innerExpr.(*expressions.SelectExpression)
	if !ok {
		return
	}

	// Only fire on inner joins with exactly 2 ForEach quantifiers.
	if sel.GetJoinType() != expressions.JoinInner {
		return
	}
	quantifiers := sel.GetQuantifiers()
	if len(quantifiers) != 2 {
		return
	}
	if quantifiers[0].Kind() != expressions.QuantifierForEach ||
		quantifiers[1].Kind() != expressions.QuantifierForEach {
		return
	}

	aliases := sel.GetSourceAliases()
	if len(aliases) < 2 {
		return
	}

	filterPreds := f.GetPredicates()
	if len(filterPreds) == 0 {
		return
	}

	// Partition predicates into: push-to-0, push-to-1, keep-on-join.
	var pushTo0, pushTo1, keep []predicates.QueryPredicate
	for _, pred := range filterPreds {
		side := predicateSingleSide(pred, aliases[0], aliases[1])
		switch side {
		case 0:
			pushTo0 = append(pushTo0, pred)
		case 1:
			pushTo1 = append(pushTo1, pred)
		default:
			keep = append(keep, pred)
		}
	}

	// Nothing to push — rule doesn't fire.
	if len(pushTo0) == 0 && len(pushTo1) == 0 {
		return
	}

	// Build the new quantifiers, wrapping each side with a filter
	// if there are predicates to push to it.
	newQ0 := quantifiers[0]
	if len(pushTo0) > 0 {
		innerRef0 := quantifiers[0].GetRangesOver()
		pushed0 := expressions.NewLogicalFilterExpression(pushTo0,
			expressions.ForEachQuantifier(innerRef0))
		newQ0 = expressions.ForEachQuantifier(call.MemoizeExpression(pushed0))
	}

	newQ1 := quantifiers[1]
	if len(pushTo1) > 0 {
		innerRef1 := quantifiers[1].GetRangesOver()
		pushed1 := expressions.NewLogicalFilterExpression(pushTo1,
			expressions.ForEachQuantifier(innerRef1))
		newQ1 = expressions.ForEachQuantifier(call.MemoizeExpression(pushed1))
	}

	// Build the new SelectExpression with the modified quantifiers.
	newSel := expressions.NewSelectExpressionWithJoinType(
		sel.GetResultValue(),
		[]expressions.Quantifier{newQ0, newQ1},
		sel.GetPredicates(),
		aliases,
		sel.GetJoinType(),
	)

	// If all filter predicates were pushed, the filter wrapper is
	// unnecessary — yield the Select directly. Otherwise wrap in a
	// filter with the remaining predicates.
	if len(keep) == 0 {
		call.Yield(newSel)
	} else {
		selQ := expressions.ForEachQuantifier(call.MemoizeExpression(newSel))
		call.Yield(expressions.NewLogicalFilterExpression(keep, selQ))
	}
}

// predicateSingleSide returns which side of a 2-way join a predicate
// references:
//   - 0 if it only references alias0
//   - 1 if it only references alias1
//   - -1 if it references both, neither, or has no FieldValue refs
//
// The check walks the predicate's entire Value tree looking for
// FieldValue nodes whose Field starts with "ALIAS." (dot-separated).
func predicateSingleSide(pred predicates.QueryPredicate, alias0, alias1 string) int {
	prefix0 := strings.ToUpper(alias0) + "."
	prefix1 := strings.ToUpper(alias1) + "."

	refs0, refs1 := false, false
	foundAnyField := false

	walkPredicateFieldValues(pred, func(fv *values.FieldValue) {
		foundAnyField = true
		upper := strings.ToUpper(fv.Field)
		if strings.HasPrefix(upper, prefix0) {
			refs0 = true
		}
		if strings.HasPrefix(upper, prefix1) {
			refs1 = true
		}
	})

	if !foundAnyField {
		return -1 // conservative: no field references → keep on join
	}
	if refs0 && !refs1 {
		return 0
	}
	if refs1 && !refs0 {
		return 1
	}
	return -1 // both or neither
}

// walkPredicateFieldValues walks all Value trees reachable from a
// predicate, calling visit for each FieldValue found.
func walkPredicateFieldValues(pred predicates.QueryPredicate, visit func(*values.FieldValue)) {
	predicates.WalkPredicate(pred, func(node predicates.QueryPredicate) bool {
		switch np := node.(type) {
		case *predicates.ValuePredicate:
			walkValueForFieldValues(np.Value, visit)
		case *predicates.ComparisonPredicate:
			walkValueForFieldValues(np.Operand, visit)
			walkValueForFieldValues(np.Comparison.Operand, visit)
		}
		return true
	})
}

// walkValueForFieldValues walks a Value tree, calling visit for each
// FieldValue found.
func walkValueForFieldValues(v values.Value, visit func(*values.FieldValue)) {
	values.WalkValue(v, func(node values.Value) bool {
		if fv, ok := node.(*values.FieldValue); ok {
			visit(fv)
		}
		return true
	})
}

var _ ExpressionRule = (*PushFilterBelowJoinRule)(nil)
