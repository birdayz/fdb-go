package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/combinatorics"
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
// RichOrdering to match explode aliases to equality-bound ordering keys.
// For each FixedBinding in the ordering, the comparison's
// GetCorrelatedTo() identifies the explode alias. Matched explodes
// become sorted IN-sources placed outermost in the InJoin chain,
// exploiting the inner plan's index ordering. Unmatched explodes use
// default (unsorted) quantifier order.
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

		allOrderings := r.enumerateSourceOrderings(
			innerExprs, explodeQuantifiers, explodeAliases, explodeAliasMap)

		for _, orderedSources := range allOrderings {
			currentRef := call.MemoizeFinalExpressionsFromOther(innerRef, innerExprs)
			currentPlan := plans.RecordQueryPlan(innerPlans[0])

			for i := len(orderedSources) - 1; i >= 0; i-- {
				source := orderedSources[i]
				inJoinPlan := plans.NewRecordQueryInJoinPlan(
					currentPlan, source.bindingName, source.sorted, source.reverse)
				wrapper := NewPhysicalInJoinWrapper(inJoinPlan,
					expressions.NewPhysicalQuantifier(currentRef))
				currentRef = call.MemoizeFinalExpression(wrapper)
				currentPlan = inJoinPlan
			}

			for _, m := range currentRef.FinalMembers() {
				call.YieldFinalExpression(m)
			}
		}
	}
}

type inJoinSource struct {
	bindingName string
	sorted      bool
	reverse     bool
	quantifier  expressions.Quantifier
}

// enumerateSourceOrderings returns all valid source orderings.
// The prefix (ordering-correlated sources) is fixed; the remaining
// sources are permuted using TopologicalSort.Permutations.
func (r *ImplementInJoinRule) enumerateSourceOrderings(
	innerExprs []expressions.RelationalExpression,
	explodeQuantifiers []expressions.Quantifier,
	explodeAliases map[values.CorrelationIdentifier]struct{},
	explodeAliasMap map[values.CorrelationIdentifier]expressions.Quantifier,
) [][]inJoinSource {
	var richOrdering *RichOrdering
	for _, expr := range innerExprs {
		if ph, ok := expr.(physicalPlanExpression); ok {
			richOrdering = computeWrapperRichOrdering(ph)
			break
		}
	}

	if richOrdering == nil || len(richOrdering.GetKeys()) == 0 {
		return r.enumerateDefaultSources(explodeQuantifiers)
	}

	var prefix []inJoinSource
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
			eqComp := cr.GetEqualityComparison()
			if eqComp == nil {
				continue
			}
			correlated := eqComp.GetCorrelatedTo()
			if len(correlated) != 1 {
				continue
			}
			for alias := range correlated {
				if _, isExplode := explodeAliases[alias]; !isExplode {
					continue
				}
				if _, alreadyUsed := used[alias]; alreadyUsed {
					continue
				}
				eq := explodeAliasMap[alias]
				prefix = append(prefix, inJoinSource{
					bindingName: alias.String(),
					sorted:      true,
					quantifier:  eq,
				})
				used[alias] = struct{}{}
			}
		}
	}

	var remaining []inJoinSource
	for _, eq := range explodeQuantifiers {
		alias := eq.GetAlias()
		if _, ok := used[alias]; !ok {
			remaining = append(remaining, inJoinSource{
				bindingName: alias.String(),
				quantifier:  eq,
			})
		}
	}

	if len(remaining) <= 1 {
		result := make([]inJoinSource, 0, len(prefix)+len(remaining))
		result = append(result, prefix...)
		result = append(result, remaining...)
		return [][]inJoinSource{result}
	}

	remainingNames := make([]string, len(remaining))
	nameToSource := make(map[string]inJoinSource, len(remaining))
	for i, s := range remaining {
		remainingNames[i] = s.bindingName
		nameToSource[s.bindingName] = s
	}

	iter := combinatorics.Permutations(remainingNames)
	var results [][]inJoinSource
	for {
		perm := iter.Next()
		if perm == nil {
			break
		}
		result := make([]inJoinSource, 0, len(prefix)+len(perm))
		result = append(result, prefix...)
		for _, name := range perm {
			result = append(result, nameToSource[name])
		}
		results = append(results, result)
	}
	return results
}

func (r *ImplementInJoinRule) enumerateDefaultSources(explodeQuantifiers []expressions.Quantifier) [][]inJoinSource {
	if len(explodeQuantifiers) <= 1 {
		sources := make([]inJoinSource, len(explodeQuantifiers))
		for i, eq := range explodeQuantifiers {
			sources[i] = inJoinSource{
				bindingName: eq.GetAlias().String(),
				quantifier:  eq,
			}
		}
		return [][]inJoinSource{sources}
	}

	names := make([]string, len(explodeQuantifiers))
	nameToSource := make(map[string]inJoinSource, len(explodeQuantifiers))
	for i, eq := range explodeQuantifiers {
		name := eq.GetAlias().String()
		names[i] = name
		nameToSource[name] = inJoinSource{
			bindingName: name,
			quantifier:  eq,
		}
	}

	iter := combinatorics.Permutations(names)
	var results [][]inJoinSource
	for {
		perm := iter.Next()
		if perm == nil {
			break
		}
		result := make([]inJoinSource, len(perm))
		for i, name := range perm {
			result[i] = nameToSource[name]
		}
		results = append(results, result)
	}
	return results
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
