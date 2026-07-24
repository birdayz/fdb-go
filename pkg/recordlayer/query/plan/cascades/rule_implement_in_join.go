package cascades

import (
	"context"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/combinatorics"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
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
		matcher: NewExpressionMatcher[*expressions.SelectExpression]("implement_in_join"),
	}
}

func (r *ImplementInJoinRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *ImplementInJoinRule) OnMatch(call *ImplementationRuleCall) {
	if call.IsConstraintOnly() || call.CancellationErr() != nil {
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
	// IN-join chains have no strict scalar compensation. A malformed/future
	// strict+Explode shape must stay unimplemented rather than bypass the sole
	// FirstOrDefault authority in ImplementNestedLoopJoinRule.
	if hasStrictSingleQuantifier(quantifiers) {
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
		requestedOrderings = []*properties.RequestedOrdering{properties.PreserveOrdering()}
	} else {
		hasPreserve := false
		for _, ro := range requestedOrderings {
			if ro.IsPreserve() {
				hasPreserve = true
				break
			}
		}
		if !hasPreserve {
			requestedOrderings = append(requestedOrderings, properties.PreserveOrdering())
		}
	}

	for _, partition := range partitions {
		if call.CancellationErr() != nil {
			return
		}
		innerPlans := partition.GetPlans()
		if len(innerPlans) == 0 {
			continue
		}

		// GetPhysicalExpressions, not GetExpressions: innerPlans below comes from
		// GetPlans, which FILTERS to physical members, while GetExpressions does
		// not. Seeding a FINAL reference from the unfiltered list could memoize a
		// non-physical member as a plan alternative, and the two lists are used
		// together here — one seeds the memo reference, the other drives the plan
		// chain.
		//
		// Latent, not live: instrumenting this site over the full 2407-query
		// corpus found ZERO partitions where the two lists differ, which is why
		// InJoin reports no unreachable edges. Fixed rather than left as a
		// comment because the helper exists precisely for this pairing and had no
		// caller.
		innerExprs := partition.GetPhysicalExpressions()

		for _, requestedOrdering := range requestedOrderings {
			if call.CancellationErr() != nil {
				return
			}
			allOrderings := r.enumerateSourceOrderingsForRequestedOrdering(
				call.RunContext, innerExprs, explodeQuantifiers, explodeAliases, explodeAliasMap,
				requestedOrdering)

			for _, orderedSources := range allOrderings {
				if call.CancellationErr() != nil {
					return
				}
				// Each innerPlans pass re-memoizes the SAME innerExprs group and
				// builds structurally-identical InJoins that dedup in the memo; the
				// specific member no longer seeds a plan snapshot (RFC-184 W2), so
				// only the iteration count is consulted here.
				for range innerPlans {
					if call.CancellationErr() != nil {
						return
					}
					currentRef := call.MemoizeFinalExpressionsFromOther(innerRef, innerExprs)

					for i := len(orderedSources) - 1; i >= 0; i-- {
						if call.CancellationErr() != nil {
							return
						}
						source := orderedSources[i]
						// The InJoin is its own cascades expression carrying the live
						// currentRef inner edge (RFC-184 W2); its per-ordering winner
						// resolves at extraction via ref.Winner(). No plan snapshot —
						// the deferred-winner case.
						inJoinPlan := plans.NewRecordQueryInJoinPlanFromQuantifier(
							expressions.NewPhysicalQuantifier(currentRef),
							source.bindingName, source.sorted, source.reverse)
						if inValues := extractInValues(source.quantifier); inValues != nil {
							inJoinPlan.SetInValues(inValues)
						}
						inJoinPlan.SetSourceKind(classifyInSourceKind(source.quantifier))
						currentRef = call.MemoizeFinalExpression(inJoinPlan)
					}

					for _, m := range currentRef.AllMembers() {
						if call.CancellationErr() != nil {
							return
						}
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
	runCtx context.Context,
	innerExprs []expressions.RelationalExpression,
	explodeQuantifiers []expressions.Quantifier,
	explodeAliases map[values.CorrelationIdentifier]struct{},
	explodeAliasMap map[values.CorrelationIdentifier]expressions.Quantifier,
	requestedOrdering *properties.RequestedOrdering,
) [][]inJoinSource {
	if runCtx != nil && runCtx.Err() != nil {
		return nil
	}
	var richOrdering *properties.RichOrdering
	for _, expr := range innerExprs {
		if runCtx != nil && runCtx.Err() != nil {
			return nil
		}
		if ph, ok := expr.(physicalPlanExpression); ok {
			richOrdering = computeWrapperRichOrdering(ph)
			break
		}
	}

	if richOrdering == nil || len(richOrdering.GetKeys()) == 0 {
		return r.enumerateDefaultSources(runCtx, explodeQuantifiers)
	}

	if requestedOrdering.IsPreserve() || requestedOrdering.Size() == 0 {
		return r.buildSourcesFromProvided(runCtx, richOrdering, explodeQuantifiers, explodeAliases, explodeAliasMap)
	}

	var prefix []inJoinSource
	available := make(map[values.CorrelationIdentifier]struct{})
	for k, v := range explodeAliases {
		available[k] = v
	}

	reqParts := requestedOrdering.GetParts()
	for i := 0; i < len(reqParts) && len(available) > 0; i++ {
		if runCtx != nil && runCtx.Err() != nil {
			return nil
		}
		part := reqParts[i]
		bindings := richOrdering.GetBindingMap()[part.Value]
		if len(bindings) == 0 {
			return nil
		}

		sortOrder := properties.SortOrderOf(bindings)
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
		if part.SortOrder.IsAnyDescending() {
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

	return r.appendRemaining(runCtx, prefix, explodeQuantifiers, available)
}

// buildSourcesFromProvided walks the provided ordering (fallback when no
// requested ordering is given).
func (r *ImplementInJoinRule) buildSourcesFromProvided(
	runCtx context.Context,
	richOrdering *properties.RichOrdering,
	explodeQuantifiers []expressions.Quantifier,
	explodeAliases map[values.CorrelationIdentifier]struct{},
	explodeAliasMap map[values.CorrelationIdentifier]expressions.Quantifier,
) [][]inJoinSource {
	if runCtx != nil && runCtx.Err() != nil {
		return nil
	}
	var prefix []inJoinSource
	used := make(map[values.CorrelationIdentifier]struct{})

	for _, key := range richOrdering.GetKeys() {
		if runCtx != nil && runCtx.Err() != nil {
			return nil
		}
		bindings := richOrdering.GetBindingMap()[key]
		if !properties.AreAllBindingsFixed(bindings) {
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
	return r.appendRemaining(runCtx, prefix, explodeQuantifiers, available)
}

func (r *ImplementInJoinRule) appendRemaining(
	runCtx context.Context,
	prefix []inJoinSource,
	explodeQuantifiers []expressions.Quantifier,
	available map[values.CorrelationIdentifier]struct{},
) [][]inJoinSource {
	if runCtx != nil && runCtx.Err() != nil {
		return nil
	}
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
		if runCtx != nil && runCtx.Err() != nil {
			return nil
		}
		perm := iter.Next()
		if perm == nil {
			break
		}
		result := make([]inJoinSource, 0, len(prefix)+len(perm))
		result = append(result, prefix...)
		for _, name := range perm {
			if runCtx != nil && runCtx.Err() != nil {
				return nil
			}
			result = append(result, nameToSource[name])
		}
		results = append(results, result)
	}
	return results
}

func (r *ImplementInJoinRule) enumerateDefaultSources(
	runCtx context.Context,
	explodeQuantifiers []expressions.Quantifier,
) [][]inJoinSource {
	if runCtx != nil && runCtx.Err() != nil {
		return nil
	}
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
		if runCtx != nil && runCtx.Err() != nil {
			return nil
		}
		perm := iter.Next()
		if perm == nil {
			break
		}
		result := make([]inJoinSource, len(perm))
		for i, name := range perm {
			if runCtx != nil && runCtx.Err() != nil {
				return nil
			}
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
