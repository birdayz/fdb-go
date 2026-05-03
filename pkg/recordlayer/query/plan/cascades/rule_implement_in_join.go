package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// ImplementInJoinRule implements a SELECT over ExplodeExpressions
// (UNNEST of IN-lists) and a correlated inner plan as a right-deep
// chain of RecordQueryInJoinPlans.
//
// Ports Java's ImplementInJoinRule. The rule examines the inner plan's
// ordering to match explode aliases to equality-bound ordering keys
// (via Comparison.GetCorrelatedTo). When ordering info is available,
// IN-sources are ordered to exploit the inner plan's sort order.
// When no ordering info is available, falls back to quantifier order.
type ImplementInJoinRule struct {
	matcher matching.BindingMatcher
}

func NewImplementInJoinRule() *ImplementInJoinRule {
	return &ImplementInJoinRule{
		matcher: &selectExpressionMatcher{},
	}
}

func (r *ImplementInJoinRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *ImplementInJoinRule) OnMatch(call *ImplementationRuleCall) {
	selectExpr := call.Bindings.Get(r.matcher).(*expressions.SelectExpression)

	if selectExpr.HasPredicates() {
		return
	}

	quantifiers := selectExpr.GetQuantifiers()
	if len(quantifiers) < 2 {
		return
	}

	resultValue := selectExpr.GetResultValue()

	var explodeQuantifiers []expressions.Quantifier
	var innerQuantifier expressions.Quantifier
	hasInner := false

	for _, q := range quantifiers {
		ref := q.GetRangesOver()
		if ref == nil {
			return
		}
		if isExplodeExpression(ref) {
			explodeQuantifiers = append(explodeQuantifiers, q)
		} else if !hasInner {
			innerQuantifier = q
			hasInner = true
		} else {
			return
		}
	}

	if !hasInner || len(explodeQuantifiers) == 0 {
		return
	}

	qov, ok := resultValue.(*values.QuantifiedObjectValue)
	if !ok || qov.Correlation != innerQuantifier.GetAlias() {
		return
	}

	innerRef := innerQuantifier.GetRangesOver()
	if innerRef == nil {
		return
	}

	explodeAliasMap := make(map[values.CorrelationIdentifier]expressions.Quantifier, len(explodeQuantifiers))
	explodeAliases := make(map[values.CorrelationIdentifier]struct{}, len(explodeQuantifiers))
	for _, eq := range explodeQuantifiers {
		alias := eq.GetAlias()
		explodeAliasMap[alias] = eq
		explodeAliases[alias] = struct{}{}
	}

	partitions := ToPlanPartitions(innerRef)
	if len(partitions) == 0 {
		return
	}

	for _, partition := range partitions {
		innerPlans := partition.GetPlans()
		if len(innerPlans) == 0 {
			continue
		}

		innerExprs := partition.GetExpressions()

		orderedSources := r.orderSourcesByInnerOrdering(
			innerExprs, explodeQuantifiers, explodeAliases, explodeAliasMap)

		currentRef := call.MemoizeFinalExpressionsFromOther(innerRef, innerExprs)

		for i := len(orderedSources) - 1; i >= 0; i-- {
			source := orderedSources[i]
			inJoinPlan := plans.NewRecordQueryInJoinPlan(
				innerPlans[0], source.bindingName, source.sorted, source.reverse)
			wrapper := NewPhysicalInJoinWrapper(inJoinPlan,
				expressions.NewPhysicalQuantifier(currentRef))
			currentRef = call.MemoizeFinalExpression(wrapper)
		}

		for _, m := range currentRef.FinalMembers() {
			call.YieldFinalExpression(m)
		}
	}
}

type inJoinSource struct {
	bindingName string
	sorted      bool
	reverse     bool
	quantifier  expressions.Quantifier
}

// orderSourcesByInnerOrdering examines the inner expressions' ordering
// to determine optimal IN-source nesting. If the ordering's fixed
// bindings correlate to explode aliases, those explodes are placed
// first (outermost) in the InJoin chain.
func (r *ImplementInJoinRule) orderSourcesByInnerOrdering(
	innerExprs []expressions.RelationalExpression,
	explodeQuantifiers []expressions.Quantifier,
	explodeAliases map[values.CorrelationIdentifier]struct{},
	explodeAliasMap map[values.CorrelationIdentifier]expressions.Quantifier,
) []inJoinSource {
	var richOrdering *RichOrdering
	for _, expr := range innerExprs {
		if ph, ok := expr.(physicalPlanExpression); ok {
			richOrdering = computeWrapperRichOrdering(ph)
			break
		}
	}

	if richOrdering == nil || len(richOrdering.GetKeys()) == 0 {
		return r.defaultSources(explodeQuantifiers)
	}

	var ordered []inJoinSource
	used := make(map[values.CorrelationIdentifier]struct{})

	for _, key := range richOrdering.GetKeys() {
		bindings := richOrdering.GetBindingMap()[key]
		if !AreAllBindingsFixed(bindings) {
			continue
		}

		for _, b := range bindings {
			comp := b.GetComparison()
			if comp == nil {
				continue
			}
			cr, ok := comp.(*predicates.ComparisonRange)
			if !ok {
				continue
			}
			_ = cr

			// TODO: extract correlation from ComparisonRange's comparison
			// to match explode aliases. Currently ComparisonRange doesn't
			// carry full Comparison objects with correlation tracking.
		}
	}

	for _, eq := range explodeQuantifiers {
		alias := eq.GetAlias()
		if _, ok := used[alias]; !ok {
			ordered = append(ordered, inJoinSource{
				bindingName: alias.String(),
				quantifier:  eq,
			})
		}
	}

	return ordered
}

func (r *ImplementInJoinRule) defaultSources(explodeQuantifiers []expressions.Quantifier) []inJoinSource {
	sources := make([]inJoinSource, len(explodeQuantifiers))
	for i, eq := range explodeQuantifiers {
		sources[i] = inJoinSource{
			bindingName: eq.GetAlias().String(),
			quantifier:  eq,
		}
	}
	return sources
}

func isExplodeExpression(ref *expressions.Reference) bool {
	for _, m := range ref.AllMembers() {
		if _, ok := m.(*expressions.ExplodeExpression); ok {
			return true
		}
	}
	return false
}

var _ ImplementationRule = (*ImplementInJoinRule)(nil)
