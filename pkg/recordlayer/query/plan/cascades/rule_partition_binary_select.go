package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// PartitionBinarySelectRule handles the special case of a
// SelectExpression with exactly 2 quantifiers. It rearranges
// predicates so that each predicate is evaluated at its leftmost
// possible position, creating sub-SelectExpressions that absorb
// predicates that can be pushed down.
//
// For each ordering (left, right) and (right, left), the rule
// attempts to push predicates toward the "left" side (which will
// become the outer of a nested loop join). Join predicates
// (referencing both sides) go to the right side (inner), since
// the left must be planned first. This creates a correlation from
// right to left, forcing the right side to be planned as the inner
// of a nested-loop join.
//
// Three cases:
//  1. One leg is correlated to the other — the correlated-to leg must
//     be planned as the outer. The rule only produces the valid ordering.
//  2. No correlation but join predicates — predicates are pushed to one
//     side, creating a new correlation. Both orderings are explored.
//  3. Completely independent — predicates go to whichever side they
//     correlate to. Both orderings are explored (producing equivalent results).
//
// The rule fires twice per pair of quantifiers (once for each assignment
// of left/right), since it matches quantifiers via exactlyInAnyOrder.
// Go implements this by iterating over both orderings explicitly.
//
// Ports Java's PartitionBinarySelectRule (ExplorationCascadesRule).
type PartitionBinarySelectRule struct {
	matcher matching.BindingMatcher
}

func NewPartitionBinarySelectRule() *PartitionBinarySelectRule {
	return &PartitionBinarySelectRule{
		matcher: NewExpressionMatcher[*expressions.SelectExpression]("partition_binary_select"),
	}
}

func (r *PartitionBinarySelectRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *PartitionBinarySelectRule) OnMatch(call *ExpressionRuleCall) {
	sel := matching.Get[*expressions.SelectExpression](call.Bindings, r.matcher)
	quantifiers := sel.GetQuantifiers()

	if len(quantifiers) != 2 {
		return
	}

	// Only partition binary joins (both ForEach). Existential quantifiers
	// (EXISTS subqueries) have special alias semantics that break when
	// wrapped in sub-SelectExpressions — the ExistsPredicate references
	// the existential's alias, and wrapping would change the structure
	// downstream rules expect. Java handles this in the exploration
	// phase where Memo-level dedup prevents interference; Go's fixpoint
	// architecture requires an explicit guard.
	for _, q := range quantifiers {
		if q.Kind() != expressions.QuantifierForEach {
			return
		}
	}

	// Idempotency guard: if the Reference already contains a 2-quantifier
	// SelectExpression with no predicates, this rule (or a previous
	// iteration) has already pushed predicates into sub-SelectExpressions.
	// Without this guard, the cycle PartitionBinary → SelectMerge →
	// PartitionBinary causes infinite memo growth (MaxTasks cap hit).
	for _, m := range call.Reference.Members() {
		if other, ok := m.(*expressions.SelectExpression); ok && other != sel {
			if len(other.GetQuantifiers()) == 2 && !other.HasPredicates() {
				return
			}
		}
	}

	// Try both orderings: (q0 as left, q1 as right) and (q1 as left, q0 as right).
	// Java achieves this via the exactlyInAnyOrder matcher which binds both permutations.
	for _, ordering := range [][2]int{{0, 1}, {1, 0}} {
		leftQ := quantifiers[ordering[0]]
		rightQ := quantifiers[ordering[1]]
		r.tryPartition(call, sel, leftQ, rightQ)
	}
}

func (r *PartitionBinarySelectRule) tryPartition(
	call *ExpressionRuleCall,
	sel *expressions.SelectExpression,
	leftQuantifier, rightQuantifier expressions.Quantifier,
) {
	leftAlias := leftQuantifier.GetAlias()
	rightAlias := rightQuantifier.GetAlias()

	fullCorrelationOrder := computeTransitiveCorrelationOrder(sel.GetQuantifiers())

	leftDeps := fullCorrelationOrder[leftAlias]
	if _, leftDependsOnRight := leftDeps[rightAlias]; leftDependsOnRight {
		return
	}

	var leftPredicates []predicates.QueryPredicate
	var rightPredicates []predicates.QueryPredicate

	for _, pred := range sel.GetPredicates() {
		correlatedTo := predicates.GetCorrelatedToOfPredicate(pred)
		if _, ok := correlatedTo[rightAlias]; ok {
			rightPredicates = append(rightPredicates, pred)
		} else {
			leftPredicates = append(leftPredicates, pred)
		}
	}

	// Only proceed if the partitioning is useful.
	if len(leftPredicates) == 0 && len(rightPredicates) == 0 {
		return
	}

	// Build the new left quantifier (possibly wrapped in a new Select
	// with absorbed predicates).
	var newLeftQuantifier expressions.Quantifier
	if len(leftPredicates) == 0 {
		newLeftQuantifier = leftQuantifier
	} else {
		leftBuilder := NewGraphExpansionBuilder()
		leftBuilder.AddQuantifier(leftQuantifier)
		for _, p := range leftPredicates {
			leftBuilder.AddPredicate(p)
		}

		var leftSelectExpr *expressions.SelectExpression
		if leftQuantifier.Kind() == expressions.QuantifierForEach {
			// buildSimpleSelectOverQuantifier: result value = quantifier's
			// flowed object value.
			leftSelectExpr = leftBuilder.Build().Seal().BuildSelectWithResultValue(
				leftQuantifier.GetFlowedObjectValue(),
			)
		} else {
			leftBuilder.AddColumn("", values.LiteralValue(int64(1)))
			leftSelectExpr = leftBuilder.Build().Seal().BuildSelect()
		}
		newLeftQuantifier = expressions.NamedForEachQuantifier(
			leftQuantifier.GetAlias(),
			call.MemoizeExpression(leftSelectExpr),
		)
	}

	// Build the new right quantifier (possibly wrapped in a new Select
	// with absorbed predicates).
	var newRightQuantifier expressions.Quantifier
	if len(rightPredicates) == 0 {
		newRightQuantifier = rightQuantifier
	} else {
		rightBuilder := NewGraphExpansionBuilder()
		rightBuilder.AddQuantifier(rightQuantifier)
		for _, p := range rightPredicates {
			rightBuilder.AddPredicate(p)
		}

		var rightSelectExpr *expressions.SelectExpression
		if rightQuantifier.Kind() == expressions.QuantifierForEach {
			rightSelectExpr = rightBuilder.Build().Seal().BuildSelectWithResultValue(
				rightQuantifier.GetFlowedObjectValue(),
			)
		} else {
			rightBuilder.AddColumn("", values.LiteralValue(int64(1)))
			rightSelectExpr = rightBuilder.Build().Seal().BuildSelect()
		}
		newRightQuantifier = expressions.NamedForEachQuantifier(
			rightQuantifier.GetAlias(),
			call.MemoizeExpression(rightSelectExpr),
		)
	}

	// Propagate sourceAliases in the new quantifier order. After alias
	// unification, quantifier aliases ARE the source aliases.
	la := leftQuantifier.GetAlias().Name()
	ra := rightQuantifier.GetAlias().Name()
	var newAliases []string
	if la != "" && ra != "" {
		newAliases = []string{la, ra}
	}

	newSelectExpr := expressions.NewSelectExpressionWithJoinType(
		sel.GetResultValue(),
		[]expressions.Quantifier{newLeftQuantifier, newRightQuantifier},
		nil,
		newAliases,
		sel.GetJoinType(),
	)

	call.Yield(newSelectExpr)
}

var _ ExpressionRule = (*PartitionBinarySelectRule)(nil)
