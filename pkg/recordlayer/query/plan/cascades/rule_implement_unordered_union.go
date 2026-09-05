package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// ImplementUnorderedUnionRule implements LogicalUnionExpression as a
// RecordQueryUnorderedUnionPlan: each leg's plan partition is memoized into a
// physical quantifier and the union is yielded over those quantifiers.
//
// Ports Java's ImplementUnorderedUnionRule, and does no more than Java's
// does. In particular it does not compare or rename the legs' column names:
// a union's legs are aligned once, by the translator, onto one exact row
// (exactUnionResultRow / normalizeUnionLeg), and the physical union's
// constructor asserts that every leg flows that row. A leg that disagrees
// fails the call loudly there. Re-deriving names below the translator is how
// the same name came to be compared under two spellings (RFC-242).
type ImplementUnorderedUnionRule struct {
	matcher matching.BindingMatcher
}

func NewImplementUnorderedUnionRule() *ImplementUnorderedUnionRule {
	return &ImplementUnorderedUnionRule{
		matcher: NewExpressionMatcher[*expressions.LogicalUnionExpression]("implement_unordered_union"),
	}
}

func (r *ImplementUnorderedUnionRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *ImplementUnorderedUnionRule) OnMatch(call *ImplementationRuleCall) {
	expr := call.Bindings.Get(r.matcher).(*expressions.LogicalUnionExpression)

	quantifiers := expr.GetQuantifiers()
	if len(quantifiers) == 0 {
		return
	}

	childPartitions := make([][]*PlanPartition, len(quantifiers))
	for i, q := range quantifiers {
		// Java's matcher is all(forEachQuantifierOverRef(...)): a union whose
		// legs are not all for-each quantifiers is not this rule's to implement.
		// A concatenating union over an existential leg would emit that leg's
		// rows, which is not what an existential quantifier flows.
		if q.Kind() != expressions.QuantifierForEach {
			return
		}
		ref := q.GetRangesOver()
		if ref == nil {
			return
		}
		parts := ToPlanPartitions(ref)
		rolled := RollUpPlanPartitions(parts)
		if len(rolled) == 0 {
			return
		}
		childPartitions[i] = rolled
	}

	for _, partitions := range crossProductPartitions(childPartitions) {
		newQuantifiers := make([]expressions.Quantifier, 0, len(partitions))

		// A union carries EVERY leg or it is not this union. A leg whose
		// partition holds no expression cannot be represented, and skipping it
		// would emit a narrower union that silently drops that leg's rows —
		// so the whole combination is declined instead.
		legMissing := false
		for i, partition := range partitions {
			planExprs := partition.GetExpressions()
			if len(planExprs) == 0 {
				legMissing = true
				break
			}

			newRef := call.MemoizeFinalExpressionsFromOther(
				quantifiers[i].GetRangesOver(),
				planExprs,
			)
			newQuantifiers = append(newQuantifiers,
				expressions.NewPhysicalQuantifier(newRef))
		}

		// Arity follows Java: RecordQueryUnorderedUnionPlan.fromQuantifiers
		// imposes no minimum, so a ONE-leg logical union implements as a
		// one-leg concat. UnionSingletonElimRule normally rewrites that shape
		// away first, but the implementation rule must not depend on another
		// rule having fired — with a Go-only two-leg floor here, a surviving
		// singleton union had no implementer at all and planning returned no
		// plan and no error.
		if legMissing || len(newQuantifiers) == 0 {
			continue
		}

		// The unordered union is its own cascades expression carrying the live
		// newQuantifiers leg edges (RFC-184 W2); each leg's per-ordering winner
		// resolves at extraction via ref.Winner().
		unionPlan, err := plans.NewRecordQueryUnorderedUnionPlanFromQuantifiers(newQuantifiers)
		if err != nil {
			call.Fail(err)
			return
		}
		call.YieldFinalExpression(unionPlan)
	}
}

var _ ImplementationRule = (*ImplementUnorderedUnionRule)(nil)

// crossProductPartitions returns the Cartesian product of per-child
// partition lists. Delegates to the generic CrossProduct.
func crossProductPartitions(childPartitions [][]*PlanPartition) [][]*PlanPartition {
	return CrossProduct(childPartitions)
}
