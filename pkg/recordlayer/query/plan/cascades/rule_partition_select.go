package cascades

import (
	"sort"
	"strings"

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
		// or the result value (the "live" set this lower must flow up).
		lowersCorrelatedToByUppers := make([]values.CorrelationIdentifier, 0)

		resultValue := sel.GetResultValue()

		// Determine which lower aliases the result value needs ("live" via the
		// result). Three cases:
		//   - The flat seed's translator-built JoinMergeResultValue names only two
		//     arbitrary aliases and hides the real projection (it lives in the
		//     Project above). The rule cannot see which columns are needed, so it
		//     conservatively keeps ALL lower aliases live. This applies only at the
		//     top of the re-enumeration.
		//   - A re-enumerated JoinMergeAllValue lists EXACTLY the aliases its parent
		//     needs (GetCorrelatedToOfValue returns them). Keep only those — flowing
		//     all would generate far more distinct merge sub-products than needed,
		//     blowing up the search space.
		//   - Any other result value (a bare projection) marks live only the lowers
		//     it actually references.
		// (RFC-043.)
		_, resultIsSeedMerge := resultValue.(*values.JoinMergeResultValue)
		if resultIsSeedMerge {
			for a := range lowerAliases {
				lowersCorrelatedToByUppers = append(lowersCorrelatedToByUppers, a)
			}
		} else {
			resultCorrelatedToLowers := intersectAliases(lowerAliases, values.GetCorrelatedToOfValue(resultValue))
			for a := range resultCorrelatedToLowers {
				lowersCorrelatedToByUppers = append(lowersCorrelatedToByUppers, a)
			}
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
					// Spanning predicate — references BOTH partition halves. It
					// cannot be evaluated in the lower (its upper aliases are not
					// bound there), so it goes to the upper, which correlates to
					// the lower's flowed columns. Fold the lower aliases it touches
					// into lowersCorrelatedToByUppers (the live set) so the lower
					// flows exactly those columns. With ≥2 live lower aliases the
					// lower flows a JoinMergeAllValue (qualified ALIAS.COL keys for
					// every live table); the upper predicate then resolves the
					// lower's column through the merged row by table-qualified name,
					// no translation needed. (Go's flat-seed quantifiers carry no
					// quantifier-level correlations, so Java's uppersDependingOnLowers
					// is empty and its "can do in lower" branch would push a predicate
					// referencing an absent upper alias into the lower. RFC-043.)
					upperPredicates = append(upperPredicates, pred)
					for a := range correlatedToLower {
						lowersCorrelatedToByUppers = append(lowersCorrelatedToByUppers, a)
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

		// Dedup the lower aliases the upper correlates to (the live set — the same
		// alias can be added by both the result value and a spanning predicate).
		// Without this a lower flowing a JoinMergeAllValue would list duplicate
		// aliases.
		lowersCorrelatedToByUppers = dedupAliases(lowersCorrelatedToByUppers)

		// Skip a disconnected lower: ≥2 quantifiers that no lower predicate links
		// (a pure cross product, e.g. {A,C} for chain A—B—C or {xx,yy} for a star).
		// Its tables share no join, so the partition is a genuine cartesian product
		// — never the cost-optimal shape, and the connected associativities cover
		// the same join orders. This holds at any arity. (Java defers cross products
		// via shouldDeferCrossProducts; for a single connected component that path
		// does not fire, so Go needs this explicit guard.)
		disconnectedLower := len(lowerAliases) >= 2 && !lowerAliasesConnected(lowerAliases, lowerPredicates)
		if disconnectedLower {
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

		// If THIS select is itself an intermediate join that must flow a merged
		// row (its own result value is a JoinMergeAllValue), the upper must
		// re-stamp the merge over its OWN immediate quantifiers (the new lower
		// quantifier + the upper tables) — the original deep aliases are collapsed
		// into the lower's merged map and are no longer directly bound here. At the
		// top level the result value is the user projection and flows unchanged;
		// each table's column resolves from the final merged row by qualified name.
		// (RFC-043.)
		// Re-stamp the upper's result only when this select is an INTERMEDIATE
		// re-enumerated merge (its result is a JoinMergeAllValue). The flat seed's
		// translator-built JoinMergeResultValue is the TOP output consumed by the
		// Project above; it must flow unchanged so the Project's column derivation
		// resolves. Intermediate merges re-stamp over their immediate quantifiers
		// because the original deep aliases are collapsed into the lower's merged
		// map. (RFC-043.)
		_, parentIsMerge := resultValue.(*values.JoinMergeAllValue)
		buildUpperResult := func(newLowerAlias values.CorrelationIdentifier) values.Value {
			if !parentIsMerge {
				return resultValue
			}
			merged := make([]values.CorrelationIdentifier, 0, len(upperAliases)+1)
			merged = append(merged, newLowerAlias)
			for _, a := range allAliases {
				if _, inUpper := upperAliases[a]; inUpper {
					merged = append(merged, a)
				}
			}
			return values.NewJoinMergeAllValue(merged...)
		}

		// addUpper appends the new lower quantifier, the upper tables, and the
		// (untranslated) upper predicates to a fresh upper builder.
		addUpper := func(newLowerQ expressions.Quantifier) *GraphExpansionBuilder {
			upperBuilder := NewGraphExpansionBuilder()
			upperBuilder.AddQuantifier(newLowerQ)
			for _, a := range allAliases {
				if _, inUpper := upperAliases[a]; inUpper {
					upperBuilder.AddQuantifier(aliasToQ[a])
				}
			}
			for _, p := range upperPredicates {
				upperBuilder.AddPredicate(p)
			}
			return upperBuilder
		}

		if noLowersCorrelatedToByUpperAliases && noLowersCorrelatedToByUppers {
			// Case 1: No upper-to-lower correlation. Lower result is a
			// literal scalar 1 (cross-product style).
			lowerBuilder.AddColumn("", values.LiteralValue(int64(1)))
			lowerSelectExpr := lowerBuilder.Build().Seal().BuildSelect()

			newLowerQ := expressions.NamedForEachQuantifier(
				lowerAliasCorrelatedToByUpperAliases,
				call.MemoizeExpression(lowerSelectExpr),
			)
			upperSelectExpression = addUpper(newLowerQ).Build().Seal().
				BuildSelectWithResultValue(buildUpperResult(lowerAliasCorrelatedToByUpperAliases))

		} else if len(lowersCorrelatedToByUppers) >= 2 {
			// Merge case: ≥2 live lower tables (referenced by an upper predicate or
			// the result value). The lower flows a JoinMergeAllValue carrying
			// qualified ALIAS.COL keys for every live table; the upper predicates
			// and result value resolve those columns through the merged row by
			// table-qualified name — no translation. A chain of such merges
			// accumulates all live columns up the join spine (each merge preserves
			// already-qualified keys). Reaching here implies
			// lowersCorrelatedToByUpperAliases == 0 (the validation above skips a
			// quantifier-level upper-dep with >1 live lower). (RFC-043.)
			lowerResult := values.NewJoinMergeAllValue(lowersCorrelatedToByUppers...)
			lowerSelectExpr := lowerBuilder.Build().Seal().BuildSelectWithResultValue(lowerResult)

			// Use a STABLE alias derived from the (sorted) live set rather than a
			// fresh UniqueCorrelationIdentifier. Two partitions that produce the
			// same merged sub-join must wrap it under the same quantifier alias so
			// the upper SelectExpressions intern to the SAME memo Reference —
			// otherwise the re-enumeration's shared sub-products (e.g. the (A⋈B)
			// merge reached from many bipartitions) are re-explored per path and
			// the task count explodes super-linearly with arity. The alias is also
			// carried into buildUpperResult's JoinMergeAllValue, so a per-yield
			// unique alias there would equally defeat interning. (RFC-043.)
			mergeAlias := mergeQuantifierAlias(lowersCorrelatedToByUppers)
			newLowerQ := expressions.NamedForEachQuantifier(
				mergeAlias,
				call.MemoizeExpression(lowerSelectExpr),
			)
			upperSelectExpression = addUpper(newLowerQ).Build().Seal().
				BuildSelectWithResultValue(buildUpperResult(mergeAlias))

		} else {
			// Case 2: Exactly one live lower alias. Lower's result value is that
			// alias's flowed object value (a single table's row).
			var lowerAlias values.CorrelationIdentifier
			if len(lowersCorrelatedToByUpperAliases) == 0 {
				lowerAlias = lowersCorrelatedToByUppers[0]
			} else {
				lowerAlias = lowerAliasCorrelatedToByUpperAliases
			}

			flowedValue := aliasToQ[lowerAlias].GetFlowedObjectValue()
			lowerSelectExpr := lowerBuilder.Build().Seal().BuildSelectWithResultValue(flowedValue)

			newLowerQ := expressions.NamedForEachQuantifier(
				lowerAlias,
				call.MemoizeExpression(lowerSelectExpr),
			)
			upperSelectExpression = addUpper(newLowerQ).Build().Seal().
				BuildSelectWithResultValue(buildUpperResult(lowerAlias))
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

// mergeQuantifierAlias returns a STABLE, collision-free quantifier alias for the
// merged lower over the given live aliases. Deterministic in the live set (which
// the caller has sorted), so identical merged sub-joins reached from different
// bipartitions wrap under the same alias and intern to one memo Reference. The
// "$m_" prefix cannot collide with a table/quantifier alias (no SQL identifier
// starts with "$m_"). (RFC-043.)
func mergeQuantifierAlias(live []values.CorrelationIdentifier) values.CorrelationIdentifier {
	var b strings.Builder
	b.WriteString("$m")
	for _, a := range live {
		b.WriteByte('_')
		b.WriteString(a.Name())
	}
	return values.NamedCorrelationIdentifier(b.String())
}

var _ ExpressionRule = (*PartitionSelectRule)(nil)

// dedupAliases returns aliases with duplicates removed, sorted by name into a
// CANONICAL order. The live set (lowersCorrelatedToByUppers) is collected from
// map iteration (non-deterministic order), so it must be canonicalized for two
// reasons: (1) determinism — the JoinMergeAllValue built from it would otherwise
// vary across runs, producing non-deterministic plans; (2) memoization — two
// partitions yielding the same live set must intern to the SAME JoinMergeAllValue
// (hence the same Reference, RFC-039), or the re-enumeration's search space
// explodes with alias-permuted duplicates of identical sub-joins. (RFC-043.)
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
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
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
