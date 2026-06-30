package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/combinatorics"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
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
	if call.IsConstraintOnly() {
		return
	}
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
		if explode := getExplodeExpression(ref); explode != nil {
			if !isSupportedExplodeValue(explode.GetCollectionValue()) {
				return
			}
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

	requestedOrderings := call.GetRequestedOrderings()
	if len(requestedOrderings) == 0 {
		requestedOrderings = []*RequestedOrdering{PreserveOrdering()}
	} else {
		hasPreserve := false
		for _, ro := range requestedOrderings {
			if ro.IsPreserve() {
				hasPreserve = true
				break
			}
		}
		if !hasPreserve {
			requestedOrderings = append(requestedOrderings, PreserveOrdering())
		}
	}

	for _, partition := range partitions {
		innerPlans := partition.GetPlans()
		if len(innerPlans) == 0 {
			continue
		}

		innerExprs := partition.GetExpressions()

		for _, requestedOrdering := range requestedOrderings {
			allOrderings := r.enumerateSourceOrderingsForRequestedOrdering(
				innerExprs, explodeQuantifiers, explodeAliases, explodeAliasMap,
				requestedOrdering)

			for _, orderedSources := range allOrderings {
				for _, innerPlan := range innerPlans {
					currentRef := call.MemoizeFinalExpressionsFromOther(innerRef, innerExprs)
					currentPlan := plans.RecordQueryPlan(innerPlan)

					for i := len(orderedSources) - 1; i >= 0; i-- {
						source := orderedSources[i]
						inJoinPlan := plans.NewRecordQueryInJoinPlan(
							currentPlan, source.bindingName, source.sorted, source.reverse)
						if inValues := extractInValues(source.quantifier); inValues != nil {
							inJoinPlan.SetInValues(inValues)
						}
						inJoinPlan.SetSourceKind(classifyInSourceKind(source.quantifier))
						wrapper := NewPhysicalInJoinWrapper(inJoinPlan,
							expressions.NewPhysicalQuantifier(currentRef))
						currentRef = call.MemoizeFinalExpression(wrapper)
						currentPlan = inJoinPlan
					}

					for _, m := range currentRef.AllMembers() {
						if _, ok := m.(physicalPlanExpression); !ok {
							continue
						}
						call.YieldFinalExpression(m)
					}
				}
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

// enumerateSourceOrderingsForRequestedOrdering walks the requested
// ordering parts and matches them against the inner ordering's fixed
// bindings. Explode aliases correlated to fixed bindings become sorted
// IN-sources in the prefix. Non-explode fixed bindings are skipped.
// Remaining sources are permuted.
//
// Ports Java's ImplementInJoinRule.enumerateInSourcesForRequestedOrdering.
func (r *ImplementInJoinRule) enumerateSourceOrderingsForRequestedOrdering(
	innerExprs []expressions.RelationalExpression,
	explodeQuantifiers []expressions.Quantifier,
	explodeAliases map[values.CorrelationIdentifier]struct{},
	explodeAliasMap map[values.CorrelationIdentifier]expressions.Quantifier,
	requestedOrdering *RequestedOrdering,
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

	if requestedOrdering.IsPreserve() || requestedOrdering.Size() == 0 {
		return r.buildSourcesFromProvided(richOrdering, explodeQuantifiers, explodeAliases, explodeAliasMap)
	}

	var prefix []inJoinSource
	available := make(map[values.CorrelationIdentifier]struct{})
	for k, v := range explodeAliases {
		available[k] = v
	}

	reqParts := requestedOrdering.GetParts()
	for i := 0; i < len(reqParts) && len(available) > 0; i++ {
		part := reqParts[i]
		bindings := richOrdering.GetBindingMap()[part.Value]
		if len(bindings) == 0 {
			return nil
		}

		sortOrder := SortOrderOf(bindings)
		if sortOrder.IsDirectional() {
			return nil
		}

		var correlatedAlias values.CorrelationIdentifier
		found := false
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
				if _, isExplode := explodeAliases[alias]; isExplode {
					correlatedAlias = alias
					found = true
				}
			}
		}

		if !found {
			continue
		}

		if _, ok := available[correlatedAlias]; !ok {
			return nil
		}

		sorted := true
		reverse := false
		if part.SortOrder.IsDescending() {
			reverse = true
		}

		prefix = append(prefix, inJoinSource{
			bindingName: correlatedAlias.String(),
			sorted:      sorted,
			reverse:     reverse,
			quantifier:  explodeAliasMap[correlatedAlias],
		})
		delete(available, correlatedAlias)
	}

	return r.appendRemaining(prefix, explodeQuantifiers, available)
}

// buildSourcesFromProvided walks the provided ordering (fallback when no
// requested ordering is given).
func (r *ImplementInJoinRule) buildSourcesFromProvided(
	richOrdering *RichOrdering,
	explodeQuantifiers []expressions.Quantifier,
	explodeAliases map[values.CorrelationIdentifier]struct{},
	explodeAliasMap map[values.CorrelationIdentifier]expressions.Quantifier,
) [][]inJoinSource {
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
				prefix = append(prefix, inJoinSource{
					bindingName: alias.String(),
					sorted:      true,
					quantifier:  explodeAliasMap[alias],
				})
				used[alias] = struct{}{}
			}
		}
	}

	available := make(map[values.CorrelationIdentifier]struct{})
	for _, eq := range explodeQuantifiers {
		alias := eq.GetAlias()
		if _, ok := used[alias]; !ok {
			available[alias] = struct{}{}
		}
	}
	return r.appendRemaining(prefix, explodeQuantifiers, available)
}

func (r *ImplementInJoinRule) appendRemaining(
	prefix []inJoinSource,
	explodeQuantifiers []expressions.Quantifier,
	available map[values.CorrelationIdentifier]struct{},
) [][]inJoinSource {
	var remaining []inJoinSource
	for _, eq := range explodeQuantifiers {
		alias := eq.GetAlias()
		if _, ok := available[alias]; ok {
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

func getExplodeExpression(ref *expressions.Reference) *expressions.ExplodeExpression {
	for _, m := range ref.AllMembers() {
		if e, ok := m.(*expressions.ExplodeExpression); ok {
			return e
		}
	}
	return nil
}

// classifyInSourceKind determines the InSourceKind for an explode
// quantifier, mirroring Java's ImplementInJoinRule.computeInSource:
//   - ConstantValue (literal list) → InSourceValues
//   - QuantifiedObjectValue (parameter ref) → InSourceParameter
//   - IsConstantValue catch-all → InSourceComparand
func classifyInSourceKind(q expressions.Quantifier) plans.InSourceKind {
	ref := q.GetRangesOver()
	if ref == nil {
		return plans.InSourceValues
	}
	explode := getExplodeExpression(ref)
	if explode == nil {
		return plans.InSourceValues
	}
	cv := explode.GetCollectionValue()
	if cv == nil {
		return plans.InSourceValues
	}
	switch cv.(type) {
	case *values.ConstantValue:
		return plans.InSourceValues
	case *values.QuantifiedObjectValue:
		return plans.InSourceParameter
	default:
		if values.IsConstantValue(cv) {
			return plans.InSourceComparand
		}
		return plans.InSourceValues
	}
}

func extractInValues(q expressions.Quantifier) []any {
	ref := q.GetRangesOver()
	if ref == nil {
		return nil
	}
	explode := getExplodeExpression(ref)
	if explode == nil {
		return nil
	}
	cv := explode.GetCollectionValue()
	if cv == nil {
		return nil
	}
	// Plan-time IN-list extraction: an erroring or non-list collection value
	// declines (returns nil) rather than failing planning.
	result, err := cv.Evaluate(nil)
	if err != nil {
		return nil
	}
	if vals, ok := result.([]any); ok {
		return vals
	}
	return nil
}

func isSupportedExplodeValue(v values.Value) bool {
	if v == nil {
		return false
	}
	switch v.(type) {
	case *values.ConstantValue, *values.QuantifiedObjectValue:
		return true
	}
	return values.IsConstantValue(v)
}

var _ ImplementationRule = (*ImplementInJoinRule)(nil)
