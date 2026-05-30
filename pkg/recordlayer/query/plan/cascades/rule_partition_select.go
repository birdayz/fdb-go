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

	// The FROM-order-independent re-enumeration (routing spanning predicates to
	// the upper, flowing the correlated lower columns) is correct end-to-end only
	// for a 3-way join: the single correlated lower quantifier threads through
	// exactly ONE level of upper re-partitioning, which the executor's NLJ/FlatMap
	// path resolves. For N > 3 the correlation must survive TWO+ nested
	// re-partitions and the flattened merge loses the lower alias → wrong rows.
	// Until the nested RecordConstructorValue + TranslationMap resolution lands at
	// execution (the N-way follow-up), restrict the new behavior to 3-way and fall
	// back to Java's original classification for N > 3 — which leaves the same
	// FROM-orders unplannable (loud `could not plan query`) as before this RFC,
	// rather than returning silent wrong rows. (RFC-042 L3; scoped to 3-way per
	// Torvalds review — a loud plan-failure is acceptable, silent wrong rows are
	// not.)
	isThreeWay := n == 3

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

		for _, pred := range flattenConjuncts(sel.GetPredicates()) {
			correlatedTo := predicates.GetCorrelatedToOfPredicate(pred)
			correlatedToLower := intersectAliases(lowerAliases, correlatedTo)
			correlatedToUpper := intersectAliases(upperAliases, correlatedTo)

			if len(correlatedToUpper) > 0 {
				if len(correlatedToLower) > 0 {
					if isThreeWay {
						// Spanning predicate (3-way only) — references BOTH
						// partition halves. It cannot be evaluated in the lower
						// (its upper aliases are not bound there), so it goes to
						// the upper, which correlates to the lower's flowed
						// columns. Fold the lower aliases it touches into
						// lowersCorrelatedToByUppers so the lower flows exactly
						// those columns (Case 2/3 — Java's RecordConstructorValue +
						// TranslationMap contract), making this a valid correlated
						// join, not a degenerate lower cross-product. (Go's
						// flat-seed quantifiers carry no quantifier-level
						// correlations, so Java's uppersDependingOnLowers is empty
						// and its "can do in lower" branch would push a predicate
						// referencing an absent upper alias into the lower. The
						// single correlated lower threads through one level of
						// upper re-partitioning, which the executor resolves.
						// RFC-042 L3.)
						upperPredicates = append(upperPredicates, pred)
						for a := range correlatedToLower {
							lowersCorrelatedToByUppers = append(lowersCorrelatedToByUppers, a)
						}
					} else if intersectAliases(correlatedToUpper, uppersDependingOnLowers) == nil {
						// N > 3: original Java classification (matches master). The
						// re-enumerated correlation cannot thread through TWO+
						// nested upper re-partitions at execution yet, so keep
						// Java's "can do in lower" split. The flat seed has empty
						// uppersDependingOnLowers, so this branch always fires and
						// some FROM-orders fail to plan LOUDLY — exactly master's
						// pre-RFC behavior — instead of returning silent wrong
						// rows. (RFC-042 L3; honestly scoped to 3-way.)
						lowerPredicates = append(lowerPredicates, pred)
					} else {
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

		// Dedup the lower aliases the upper correlates to (the same alias can be
		// added by both the result value and a spanning predicate). Without this
		// the Case-3 RecordConstructorValue below would emit duplicate `_i`
		// fields.
		lowersCorrelatedToByUppers = dedupAliases(lowersCorrelatedToByUppers)

		// 3-way only: skip the two partition shapes whose lower must flow a
		// multi-alias RecordConstructorValue the executor cannot resolve against:
		//   (i)  a disconnected lower (≥2 quantifiers no lower predicate links —
		//        a pure cross product, e.g. {A,C} for chain A—B—C or {xx,yy} for a
		//        star), and
		//   (ii) a multi-alias Case-3 (upper correlates to ≥2 distinct lowers).
		// Skipping these leaves the connected single-alias Case-2 associativities,
		// which cover every join order with a lower that flows exactly one QOV the
		// executor resolves. For N > 3 these guards do not apply — the N>3
		// classification above already keeps Java's split, so such partitions fail
		// to plan loudly as on master. (RFC-042 L3.)
		disconnectedLower := len(lowerAliases) >= 2 && !lowerAliasesConnected(lowerAliases, lowerPredicates)
		multiAliasCase3 := len(lowersCorrelatedToByUppers) > 1
		if isThreeWay && (disconnectedLower || multiAliasCase3) {
			continue
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
			//
			// UNREACHABLE for n == 3: the multiAliasCase3 guard above
			// (len(lowersCorrelatedToByUppers) > 1) skips every partition that
			// would reach here while isThreeWay. This path is the N-way
			// follow-up — its nested RecordConstructorValue + TranslationMap
			// resolution at execution is what lifts the rule past 3-way (see
			// RFC-042 L3). It is kept (not deleted) as the scaffold for that work.
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
		// Use a fresh slice rather than partitions[:0]: the range below reads
		// `partitions` while we build `remaining`, and aliasing the backing
		// array is subtle to reason about. N is tiny (one entry per quantifier),
		// so a fresh allocation costs nothing and is unambiguously correct.
		newPartition := make(partition)
		var remaining []partition
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

// dedupAliases returns aliases with duplicates removed, preserving first-seen
// order (deterministic). Used to dedup lowersCorrelatedToByUppers so a lower
// alias referenced by both the result value and a spanning predicate is folded
// once (avoids duplicate RecordConstructorValue ordinal fields).
func dedupAliases(aliases []values.CorrelationIdentifier) []values.CorrelationIdentifier {
	seen := make(map[values.CorrelationIdentifier]struct{}, len(aliases))
	out := aliases[:0:0]
	for _, a := range aliases {
		if _, ok := seen[a]; ok {
			continue
		}
		seen[a] = struct{}{}
		out = append(out, a)
	}
	return out
}

// lowerAliasesConnected reports whether lowerAliases form a single connected
// component under lowerPredicates' correlations (union-find). Used to skip a
// degenerate cross-product lower whose multi-alias RecordConstructorValue result
// the executor cannot resolve translated predicates against (RFC-042 L3).
func lowerAliasesConnected(
	lowerAliases map[values.CorrelationIdentifier]struct{},
	lowerPredicates []predicates.QueryPredicate,
) bool {
	if len(lowerAliases) <= 1 {
		return true
	}
	parent := make(map[values.CorrelationIdentifier]values.CorrelationIdentifier, len(lowerAliases))
	for a := range lowerAliases {
		parent[a] = a
	}
	var find func(values.CorrelationIdentifier) values.CorrelationIdentifier
	find = func(a values.CorrelationIdentifier) values.CorrelationIdentifier {
		for parent[a] != a {
			parent[a] = parent[parent[a]]
			a = parent[a]
		}
		return a
	}
	for _, p := range lowerPredicates {
		var prev values.CorrelationIdentifier
		have := false
		for a := range intersectAliases(lowerAliases, predicates.GetCorrelatedToOfPredicate(p)) {
			if have {
				parent[find(prev)] = find(a)
			}
			prev = a
			have = true
		}
	}
	var root values.CorrelationIdentifier
	first := true
	for a := range lowerAliases {
		if first {
			root = find(a)
			first = false
			continue
		}
		if find(a) != root {
			return false
		}
	}
	return true
}
