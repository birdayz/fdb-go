package cascades

import (
	"fmt"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// PartitionSelectRule splits a SelectExpression with N >= 3 quantifiers
// into two levels: a lower SelectExpression containing a subset of the
// quantifiers (the "lower" partition) and an upper SelectExpression
// containing the remaining quantifiers plus a new ForEach quantifier
// over the lower Select. Predicates are classified by their correlations
// and distributed to the level where they can be evaluated earliest.
//
// This is the core of join enumeration in the Cascades optimizer:
// if a query has FROM a, b, c WHERE a.x = b.x AND b.y = c.y, this
// rule partitions the quantifiers into connected components (a,b and
// b,c share predicates so they form one component — or the rule
// explores every possible bipartition and lets cost decide).
//
// The rule fires once per distinct bipartition of the quantifier set.
// Each firing produces at most one yield — the upper SelectExpression.
// Convergence is guaranteed because each yielded expression has strictly
// fewer quantifiers at the top level than the input.
//
// Ports Java's PartitionSelectRule (ExplorationCascadesRule).
type PartitionSelectRule struct {
	matcher matching.BindingMatcher
}

func NewPartitionSelectRule() *PartitionSelectRule {
	return &PartitionSelectRule{
		matcher: NewExpressionMatcher[*expressions.SelectExpression]("partition_select"),
	}
}

func (r *PartitionSelectRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *PartitionSelectRule) OnMatch(call *ExpressionRuleCall) {
	sel := matching.Get[*expressions.SelectExpression](call.Bindings, r.matcher)
	quantifiers := sel.GetQuantifiers()

	if len(quantifiers) < 3 {
		return
	}

	// Only partition joins among ForEach quantifiers. Existential
	// quantifiers (EXISTS subqueries) have special alias semantics
	// that break when partitioned. See PartitionBinarySelectRule
	// for the rationale.
	for _, q := range quantifiers {
		if q.Kind() != expressions.QuantifierForEach {
			return
		}
	}

	plannerCfg := call.Context.GetPlannerConfiguration()

	// Build alias → quantifier map.
	aliasToQ := make(map[values.CorrelationIdentifier]expressions.Quantifier, len(quantifiers))
	for _, q := range quantifiers {
		aliasToQ[q.GetAlias()] = q
	}

	// Compute the full transitive correlation closure among quantifiers.
	// For each quantifier alias A, fullCorrelationOrder[A] is the set of
	// other quantifier aliases that A transitively depends on.
	fullCorrelationOrder := computeTransitiveCorrelationOrder(quantifiers)

	// Compute independent quantifiers partitioning — used to defer
	// cross products when configured to do so.
	independentPartitioning := computeIndependentQuantifiersPartitioning(sel, fullCorrelationOrder)

	// Enumerate all non-trivial bipartitions of the quantifier set.
	// "lower" is each non-empty proper subset; "upper" is the complement.
	allAliases := make([]values.CorrelationIdentifier, len(quantifiers))
	for i, q := range quantifiers {
		allAliases[i] = q.GetAlias()
	}

	// We iterate over all 2^N - 2 non-trivial subsets via bitmask.
	n := len(allAliases)
	total := 1 << n
	for mask := 1; mask < total-1; mask++ {
		lowerAliases := make(map[values.CorrelationIdentifier]struct{})
		for bit := 0; bit < n; bit++ {
			if mask&(1<<bit) != 0 {
				lowerAliases[allAliases[bit]] = struct{}{}
			}
		}

		if len(lowerAliases) == 0 {
			continue
		}

		// If right-deep only, require upper has exactly 1 quantifier.
		if plannerCfg.ShouldJoinRightDeep && len(lowerAliases) != len(quantifiers)-1 {
			continue
		}

		upperAliases := make(map[values.CorrelationIdentifier]struct{})
		for _, q := range quantifiers {
			a := q.GetAlias()
			if _, inLower := lowerAliases[a]; !inLower {
				upperAliases[a] = struct{}{}
			}
		}
		if len(upperAliases) == 0 {
			continue
		}

		// Check independent quantifiers partitioning for cross-product deferral.
		if len(independentPartitioning) > 1 {
			if plannerCfg.ShouldDeferCrossProducts {
				if !isCrossProduct(independentPartitioning, lowerAliases, upperAliases) {
					continue
				}
			}
		}

		// Reject partitioning if it would cause a dependency cycle.
		// Collect upper aliases that depend on lower aliases.
		uppersDependingOnLowers := make(map[values.CorrelationIdentifier]struct{})
		for upperAlias := range upperAliases {
			deps := fullCorrelationOrder[upperAlias]
			for lowerAlias := range lowerAliases {
				if _, ok := deps[lowerAlias]; ok {
					uppersDependingOnLowers[upperAlias] = struct{}{}
					break
				}
			}
		}

		// Check if any lower aliases depend on those upper aliases.
		cycle := false
		for lowerAlias := range lowerAliases {
			deps := fullCorrelationOrder[lowerAlias]
			for upperDep := range uppersDependingOnLowers {
				if _, ok := deps[upperDep]; ok {
					cycle = true
					break
				}
			}
			if cycle {
				break
			}
		}
		if cycle {
			continue
		}

		// Prefer right-deep DAGs: reject partitioning that would force
		// rebasing the outer side (when multiple lowers are correlated to
		// by uppers).
		lowersCorrelatedToByUpperAliases := make(map[values.CorrelationIdentifier]struct{})
		for upperAlias := range upperAliases {
			deps := fullCorrelationOrder[upperAlias]
			for lowerAlias := range lowerAliases {
				if _, ok := deps[lowerAlias]; ok {
					lowersCorrelatedToByUpperAliases[lowerAlias] = struct{}{}
				}
			}
		}
		if len(lowersCorrelatedToByUpperAliases) > 1 {
			continue
		}

		var lowerAliasCorrelatedToByUpperAliases values.CorrelationIdentifier
		if len(lowersCorrelatedToByUpperAliases) == 0 {
			lowerAliasCorrelatedToByUpperAliases = values.UniqueCorrelationIdentifier()
		} else {
			for a := range lowersCorrelatedToByUpperAliases {
				lowerAliasCorrelatedToByUpperAliases = a
				break
			}
		}

		// Track which lower aliases are referenced by upper predicates
		// or the result value.
		lowersCorrelatedToByUppers := make([]values.CorrelationIdentifier, 0)

		resultValue := sel.GetResultValue()
		resultCorrelatedToLowers := intersectAliases(lowerAliases, values.GetCorrelatedToOfValue(resultValue))
		for a := range resultCorrelatedToLowers {
			lowersCorrelatedToByUppers = append(lowersCorrelatedToByUppers, a)
		}

		// Classify predicates.
		var lowerPredicates []predicates.QueryPredicate
		var upperPredicates []predicates.QueryPredicate
		var deeplyCorrelatedPredicates []predicates.QueryPredicate

		for _, pred := range sel.GetPredicates() {
			correlatedTo := predicates.GetCorrelatedToOfPredicate(pred)
			correlatedToLower := intersectAliases(lowerAliases, correlatedTo)
			correlatedToUpper := intersectAliases(upperAliases, correlatedTo)

			if len(correlatedToUpper) > 0 {
				if len(correlatedToLower) > 0 {
					if intersectAliases(correlatedToUpper, uppersDependingOnLowers) == nil || len(intersectAliases(correlatedToUpper, uppersDependingOnLowers)) == 0 {
						// Can do in lower.
						lowerPredicates = append(lowerPredicates, pred)
					} else {
						// Must do in upper.
						upperPredicates = append(upperPredicates, pred)
						for a := range correlatedToLower {
							lowersCorrelatedToByUppers = append(lowersCorrelatedToByUppers, a)
						}
					}
				} else {
					upperPredicates = append(upperPredicates, pred)
				}
			} else {
				if len(correlatedToLower) > 0 {
					lowerPredicates = append(lowerPredicates, pred)
				} else {
					deeplyCorrelatedPredicates = append(deeplyCorrelatedPredicates, pred)
				}
			}
		}

		// Validate upper-quantifier dependency constraints.
		if len(lowersCorrelatedToByUpperAliases) > 0 {
			if len(lowersCorrelatedToByUppers) > 1 {
				continue
			}
			if len(lowersCorrelatedToByUppers) == 1 {
				if lowersCorrelatedToByUppers[0] != lowerAliasCorrelatedToByUpperAliases {
					continue
				}
			}
		}

		// Only proceed if the partitioning is useful.
		if len(lowerAliases) == 1 {
			if len(lowerPredicates) == 0 {
				continue
			}
		}

		// Build the lower GraphExpansion.
		lowerBuilder := NewGraphExpansionBuilder()
		for _, a := range allAliases {
			if _, inLower := lowerAliases[a]; inLower {
				lowerBuilder.AddQuantifier(aliasToQ[a])
			}
		}
		for _, p := range lowerPredicates {
			lowerBuilder.AddPredicate(p)
		}
		for _, p := range deeplyCorrelatedPredicates {
			lowerBuilder.AddPredicate(p)
		}

		// Build the upper SelectExpression.
		var upperSelectExpression *expressions.SelectExpression

		noLowersCorrelatedToByUpperAliases := len(lowersCorrelatedToByUpperAliases) == 0
		noLowersCorrelatedToByUppers := len(lowersCorrelatedToByUppers) == 0

		if noLowersCorrelatedToByUpperAliases && noLowersCorrelatedToByUppers {
			// Case 1: No upper-to-lower correlation. Lower result is a
			// literal scalar 1 (cross-product style).
			lowerBuilder.AddColumn("", values.LiteralValue(int64(1)))
			lowerSelectExpr := lowerBuilder.Build().Seal().BuildSelect()

			upperBuilder := NewGraphExpansionBuilder()
			newLowerQ := expressions.NamedForEachQuantifier(
				lowerAliasCorrelatedToByUpperAliases,
				call.MemoizeExpression(lowerSelectExpr),
			)
			upperBuilder.AddQuantifier(newLowerQ)
			for _, a := range allAliases {
				if _, inUpper := upperAliases[a]; inUpper {
					upperBuilder.AddQuantifier(aliasToQ[a])
				}
			}
			for _, p := range upperPredicates {
				upperBuilder.AddPredicate(p)
			}
			upperSelectExpression = upperBuilder.Build().Seal().BuildSelectWithResultValue(resultValue)

		} else if len(lowersCorrelatedToByUpperAliases) > 0 || len(lowersCorrelatedToByUppers) == 1 {
			// Case 2: Exactly one lower alias is correlated to by upper.
			// Lower's result value is that alias's flowed object value.
			var lowerAlias values.CorrelationIdentifier
			if len(lowersCorrelatedToByUpperAliases) == 0 {
				lowerAlias = lowersCorrelatedToByUppers[0]
			} else {
				lowerAlias = lowerAliasCorrelatedToByUpperAliases
			}

			flowedValue := aliasToQ[lowerAlias].GetFlowedObjectValue()
			lowerSelectExpr := lowerBuilder.Build().Seal().BuildSelectWithResultValue(flowedValue)

			upperBuilder := NewGraphExpansionBuilder()
			newLowerQ := expressions.NamedForEachQuantifier(
				lowerAlias,
				call.MemoizeExpression(lowerSelectExpr),
			)
			upperBuilder.AddQuantifier(newLowerQ)
			for _, a := range allAliases {
				if _, inUpper := upperAliases[a]; inUpper {
					upperBuilder.AddQuantifier(aliasToQ[a])
				}
			}
			for _, p := range upperPredicates {
				upperBuilder.AddPredicate(p)
			}
			upperSelectExpression = upperBuilder.Build().Seal().BuildSelectWithResultValue(resultValue)

		} else {
			// Case 3: Multiple lower aliases correlated to by uppers.
			// Build a RecordConstructorValue as the lower's result,
			// then translate upper predicates and result value via
			// ordinal-based field access through a TranslationMap.
			//
			// lowersCorrelatedToByUppers has size >= 2 here.
			lowerResultFields := make([]values.RecordConstructorField, len(lowersCorrelatedToByUppers))
			for i, la := range lowersCorrelatedToByUppers {
				lowerResultFields[i] = values.RecordConstructorField{
					Name:  fmt.Sprintf("_%d", i),
					Value: values.NewQuantifiedObjectValue(aliasToQ[la].GetAlias()),
				}
			}
			joinedResultValue := values.NewRecordConstructorValue(lowerResultFields...)
			lowerSelectExpr := lowerBuilder.Build().Seal().BuildSelectWithResultValue(joinedResultValue)

			newUpperQ := expressions.NamedForEachQuantifier(
				lowerAliasCorrelatedToByUpperAliases,
				call.MemoizeExpression(lowerSelectExpr),
			)

			// Build TranslationMap: each lower alias → FieldValue access
			// on the new upper quantifier's flowed object.
			tmBuilder := NewTranslationMapBuilder()
			for i, la := range lowersCorrelatedToByUppers {
				fieldName := fmt.Sprintf("_%d", i)
				capturedFieldName := fieldName
				capturedNewUpperQ := newUpperQ
				tmBuilder.When(la).Then(func(_ values.CorrelationIdentifier, _ values.LeafValue) values.Value {
					return values.NewFieldValue(
						values.NewQuantifiedObjectValue(capturedNewUpperQ.GetAlias()),
						capturedFieldName,
						nil, // type inferred at eval time
					)
				})
			}
			tm := tmBuilder.Build()

			// Translate upper predicates.
			newUpperPredicates := make([]predicates.QueryPredicate, len(upperPredicates))
			for i, up := range upperPredicates {
				newUpperPredicates[i] = translatePredicateCorrelations(up, tm)
			}

			// Translate result value.
			newResultValue := translateValueCorrelations(resultValue, tm)

			upperBuilder := NewGraphExpansionBuilder()
			upperBuilder.AddQuantifier(newUpperQ)
			for _, a := range allAliases {
				if _, inUpper := upperAliases[a]; inUpper {
					upperBuilder.AddQuantifier(aliasToQ[a])
				}
			}
			for _, p := range newUpperPredicates {
				upperBuilder.AddPredicate(p)
			}
			upperSelectExpression = upperBuilder.Build().Seal().BuildSelectWithResultValue(newResultValue)
		}

		call.Yield(upperSelectExpression)
	}
}

// isCrossProduct checks whether the given lower/upper partition
// aligns with the independent quantifiers partitioning. If any
// independent partition has members in BOTH lower and upper, this
// partition is not a cross product.
func isCrossProduct(
	independentPartitioning []map[values.CorrelationIdentifier]struct{},
	lowerAliases, upperAliases map[values.CorrelationIdentifier]struct{},
) bool {
	for _, partition := range independentPartitioning {
		inLower := false
		inUpper := false
		for alias := range partition {
			if _, ok := lowerAliases[alias]; ok {
				inLower = true
			} else if _, ok := upperAliases[alias]; ok {
				inUpper = true
			}
			if inLower && inUpper {
				return false
			}
		}
	}
	return true
}

// computeTransitiveCorrelationOrder computes the transitive closure
// of the depends-on relation among a set of quantifiers. For each
// quantifier alias A, the result maps A to the set of quantifier
// aliases that A transitively depends on (limited to aliases owned
// by quantifiers in the input set).
//
// This is the Go equivalent of Java's
// getCorrelationOrder().getTransitiveClosure().
//
// Implementation: builds a direct dependency map from each quantifier's
// GetCorrelatedTo() (filtering to only owned aliases), then computes
// the transitive closure via topological-order BFS.
func computeTransitiveCorrelationOrder(
	quantifiers []expressions.Quantifier,
) map[values.CorrelationIdentifier]map[values.CorrelationIdentifier]struct{} {
	// Owned aliases.
	owned := make(map[values.CorrelationIdentifier]struct{}, len(quantifiers))
	for _, q := range quantifiers {
		owned[q.GetAlias()] = struct{}{}
	}

	// Direct dependency map: alias → set of owned aliases it depends on.
	directDeps := make(map[values.CorrelationIdentifier]map[values.CorrelationIdentifier]struct{}, len(quantifiers))
	for _, q := range quantifiers {
		deps := make(map[values.CorrelationIdentifier]struct{})
		for dep := range q.GetCorrelatedTo() {
			if _, ok := owned[dep]; ok {
				deps[dep] = struct{}{}
			}
		}
		directDeps[q.GetAlias()] = deps
	}

	// Compute transitive closure via Kahn's algorithm / topological BFS.
	// Inverse map: alias → set of aliases that depend on it.
	inverse := make(map[values.CorrelationIdentifier]map[values.CorrelationIdentifier]struct{})
	inDegree := make(map[values.CorrelationIdentifier]int)
	for _, q := range quantifiers {
		a := q.GetAlias()
		if _, ok := inDegree[a]; !ok {
			inDegree[a] = 0
		}
		for dep := range directDeps[a] {
			if _, ok := inverse[dep]; !ok {
				inverse[dep] = make(map[values.CorrelationIdentifier]struct{})
			}
			inverse[dep][a] = struct{}{}
			inDegree[a]++
		}
	}

	// BFS queue starting from zero-in-degree nodes.
	result := make(map[values.CorrelationIdentifier]map[values.CorrelationIdentifier]struct{}, len(quantifiers))
	for _, q := range quantifiers {
		result[q.GetAlias()] = make(map[values.CorrelationIdentifier]struct{})
	}

	queue := make([]values.CorrelationIdentifier, 0, len(quantifiers))
	for _, q := range quantifiers {
		if inDegree[q.GetAlias()] == 0 {
			queue = append(queue, q.GetAlias())
		}
	}

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		users := inverse[current]
		for using := range users {
			inDegree[using]--
			if inDegree[using] == 0 {
				queue = append(queue, using)
				// Compute transitive deps for 'using'.
				for dep := range directDeps[using] {
					result[using][dep] = struct{}{}
					for ancestor := range result[dep] {
						result[using][ancestor] = struct{}{}
					}
				}
			}
		}
	}

	return result
}

// computeIndependentQuantifiersPartitioning computes the partitioning
// of quantifiers into independent groups. Two quantifiers are in the
// same group if they are connected by correlation dependencies or by
// shared predicates (transitively).
//
// Ports Java's SelectExpression.computeIndependentQuantifiersPartitioning.
func computeIndependentQuantifiersPartitioning(
	sel *expressions.SelectExpression,
	fullCorrelationOrder map[values.CorrelationIdentifier]map[values.CorrelationIdentifier]struct{},
) []map[values.CorrelationIdentifier]struct{} {
	quantifiers := sel.GetQuantifiers()

	// Initially: one partition per quantifier.
	type partition = map[values.CorrelationIdentifier]struct{}
	partitions := make([]partition, len(quantifiers))
	for i, q := range quantifiers {
		partitions[i] = map[values.CorrelationIdentifier]struct{}{q.GetAlias(): {}}
	}

	// Compute transitive correlation of each predicate.
	predicateTransCorr := make([]map[values.CorrelationIdentifier]struct{}, len(sel.GetPredicates()))
	for i, pred := range sel.GetPredicates() {
		corr := predicates.GetCorrelatedToOfPredicate(pred)
		transCorr := make(map[values.CorrelationIdentifier]struct{})
		for alias := range corr {
			transCorr[alias] = struct{}{}
			for ancestor := range fullCorrelationOrder[alias] {
				transCorr[ancestor] = struct{}{}
			}
		}
		predicateTransCorr[i] = transCorr
	}

	// Union-find via list manipulation — for each quantifier, merge
	// partitions that share connectivity.
	for _, q := range quantifiers {
		alias := q.GetAlias()

		connectedAliases := make(map[values.CorrelationIdentifier]struct{})
		connectedAliases[alias] = struct{}{}
		for a := range fullCorrelationOrder[alias] {
			connectedAliases[a] = struct{}{}
		}

		for _, transCorr := range predicateTransCorr {
			if _, ok := transCorr[alias]; ok {
				for a := range transCorr {
					connectedAliases[a] = struct{}{}
				}
			}
		}

		// Merge all partitions that intersect with connectedAliases.
		newPartition := make(partition)
		remaining := partitions[:0]
		for _, p := range partitions {
			if aliasesIntersect(connectedAliases, p) {
				for a := range p {
					newPartition[a] = struct{}{}
				}
			} else {
				remaining = append(remaining, p)
			}
		}
		remaining = append(remaining, newPartition)
		partitions = remaining

		if len(partitions) == 1 {
			return partitions
		}
	}

	return partitions
}

// intersectAliases returns the intersection of two alias sets.
// Returns nil if the intersection is empty.
func intersectAliases(
	a map[values.CorrelationIdentifier]struct{},
	b map[values.CorrelationIdentifier]struct{},
) map[values.CorrelationIdentifier]struct{} {
	if a == nil || b == nil {
		return nil
	}
	result := make(map[values.CorrelationIdentifier]struct{})
	for k := range a {
		if _, ok := b[k]; ok {
			result[k] = struct{}{}
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

// aliasesIntersect reports whether two alias sets have a non-empty
// intersection. Faster than intersectAliases when you only need the
// boolean answer.
func aliasesIntersect(
	a, b map[values.CorrelationIdentifier]struct{},
) bool {
	// Iterate over the smaller set.
	if len(a) > len(b) {
		a, b = b, a
	}
	for k := range a {
		if _, ok := b[k]; ok {
			return true
		}
	}
	return false
}

var _ ExpressionRule = (*PartitionSelectRule)(nil)
