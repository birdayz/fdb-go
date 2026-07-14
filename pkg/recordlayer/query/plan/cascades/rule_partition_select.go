package cascades

import (
	"fmt"
	"sort"
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
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

	// Existential quantifiers partition ONLY when there are ≥2 of them — the
	// sibling multi-EXISTS case (`WHERE EXISTS(A) AND EXISTS(B)`) that otherwise
	// STRANDS: the NLJ rule is 2-quantifier and can't match [outer, EXISTS, EXISTS],
	// and the old ForEach-only bail left it unplannable (0AF00). Peeling lower
	// {outer, EXISTS(A)} + upper {newq(outer), EXISTS(B)} produces 2-quantifier
	// existential selects the NLJ rule implements, recursing to 1-existential leaves.
	// A select with ≤1 existential is left to the existing direct-NLJ /
	// implementJoinWithExistential path (partitioning it too would race a competing
	// alternative); subsuming that Go-only arm into partitioning is separable (Java's
	// PartitionSelectRule admits any quantifier, but that migration is its own slice).
	existentialCount := 0
	for _, q := range quantifiers {
		if q.Kind() == expressions.QuantifierExistential {
			existentialCount++
		}
	}
	if existentialCount == 1 {
		return
	}
	if existentialCount >= 2 {
		// PROJECTED multi-EXISTS (`SELECT id, EXISTS(…) AS a, EXISTS(…) AS b`) is a
		// SEPARATE, harder case: the result value references the existential
		// quantifiers (booleans in the SELECT list), and peeling them into sibling
		// FlatMaps would strand the reference to the buried one. Only WHERE-EXISTS
		// existentials — semi-join FILTERS the result value does NOT reference —
		// partition here; a projected one keeps today's clean decline.
		resultCorr := values.GetCorrelatedToOfValue(sel.GetResultValue())
		for _, q := range quantifiers {
			if q.Kind() != expressions.QuantifierExistential {
				continue
			}
			if _, referenced := resultCorr[q.GetAlias()]; referenced {
				return
			}
		}
	}

	plannerCfg := call.Context.GetPlannerConfiguration()

	// A partition-built select carrying an existential quantifier needs its source
	// aliases set so ImplementNestedLoopJoinRule can resolve the inner existential's
	// correlation (it reads GetSourceAliases()[1]); the existential's source alias
	// is NOT always its quantifier alias (existsInnerCorrelation renames a
	// join/nested inner), so it must be CARRIED. GraphExpansionBuilder.BuildSelect
	// leaves source aliases empty.
	srcAliases := sel.GetSourceAliases()
	quantAliasToSource := make(map[values.CorrelationIdentifier]string, len(quantifiers))
	for i, q := range quantifiers {
		if i < len(srcAliases) {
			quantAliasToSource[q.GetAlias()] = srcAliases[i]
		}
	}
	applyExistentialSourceAliases := func(s *expressions.SelectExpression) *expressions.SelectExpression {
		if len(s.GetSourceAliases()) > 0 {
			return s
		}
		orig := s.GetQuantifiers()
		hasExistential := false
		for _, q := range orig {
			if q.Kind() == expressions.QuantifierExistential {
				hasExistential = true
				break
			}
		}
		if !hasExistential {
			return s
		}
		// ImplementNestedLoopJoinRule reads the OUTER from source-alias/quantifier
		// slot 0 and the existential INNER from slot 1, so the quantifiers must be
		// ordered [ForEach outer, Existential inner] — the shape the flat translator
		// build always emits but a partition bipartition can invert. Reorder
		// ForEach-first (a select's quantifier order is not semantically load-bearing
		// — the result value binds by alias), and set source aliases parallel.
		qs := make([]expressions.Quantifier, 0, len(orig))
		for _, q := range orig {
			if q.Kind() == expressions.QuantifierForEach {
				qs = append(qs, q)
			}
		}
		for _, q := range orig {
			if q.Kind() != expressions.QuantifierForEach {
				qs = append(qs, q)
			}
		}
		srcs := make([]string, len(qs))
		for i, q := range qs {
			if src, ok := quantAliasToSource[q.GetAlias()]; ok {
				srcs[i] = src
			} else {
				srcs[i] = q.GetAlias().Name() // a fresh lower/merge quantifier: alias IS its source
			}
		}
		return expressions.NewSelectExpressionWithAliases(s.GetResultValue(), qs, s.GetPredicates(), srcs)
	}

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

	// The select's flattened conjuncts, computed once: the classifier loop
	// consumes them per bipartition, and the disconnected-lower guard judges
	// lower connectivity against the FULL set (any predicate touching two
	// lower aliases connects them — including spanning N-ary predicates that
	// can only ever live upper).
	allPredicates := flattenConjuncts(sel.GetPredicates())

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

		// An EXISTENTIAL quantifier is a SEMI-JOIN FILTER, not a standalone relation —
		// it must be grouped WITH a ForEach outer it filters. A lower partition holding
		// an existential but NO ForEach is invalid: the existential has nothing to
		// attach to, and ImplementNestedLoopJoinRule builds a residual `QOV(inner) IS
		// NOT NULL` over a FirstOrDefault that binds to nothing → the filter drops every
		// row → silent empty result. (The UPPER always gets the new ForEach over the
		// lower, so only the lower needs this check.) Reject such a bipartition; the
		// valid peel keeps each existential with a ForEach (lower {outer, EXISTS(A)}).
		lowerHasExistential, lowerHasForEach := false, false
		for a := range lowerAliases {
			switch aliasToQ[a].Kind() {
			case expressions.QuantifierExistential:
				lowerHasExistential = true
			case expressions.QuantifierForEach:
				lowerHasForEach = true
			}
		}
		if lowerHasExistential && !lowerHasForEach {
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

		// Reject a partitioning where a LOWER quantifier has a hard QUANTIFIER-LEVEL
		// dependency on an UPPER quantifier. The lower partition is planned FIRST
		// (it becomes the inner sub-Select / FlatMap outer), so an upper alias is
		// NOT yet bound when the lower runs — a lower that genuinely DEPENDS on an
		// upper would read an unbound correlation. This direction is the
		// quantifier-correlation analog of the predicate cycle check below; it
		// matters for a multi-source lateral UNNEST whose Explode (a lower) reads
		// its array from a BURIED merge leg (an upper) — separating them
		// materializes the Explode against a row where the array key is unbound
		// (zero rows). The buried-leg dependency is supplied by
		// quantifierMergeSeedLegDeps via fullCorrelationOrder. Plain table
		// quantifiers carry no quantifier-level correlations, so this never rejects
		// an ordinary join's predicate-pushable bipartition. RFC-142.
		lowerDependsOnUpper := false
		for lowerAlias := range lowerAliases {
			deps := fullCorrelationOrder[lowerAlias]
			for upperAlias := range upperAliases {
				if _, ok := deps[upperAlias]; ok {
					lowerDependsOnUpper = true
					break
				}
			}
			if lowerDependsOnUpper {
				break
			}
		}
		if lowerDependsOnUpper {
			continue
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
		// result): exactly the lowers it references (GetCorrelatedToOfValue).
		// The name-model anchored-seed arm ("keep ALL lowers live — the anchored
		// RC hides the real projection") is DELETED with its producer (RFC-173
		// S4 item B): no plan carries an anchored RC anymore, every seed is the
		// ordinal RC whose referenced set is trustworthy.
		resultCorrelatedToLowers := intersectAliases(lowerAliases, values.GetCorrelatedToOfValue(resultValue))
		for a := range resultCorrelatedToLowers {
			lowersCorrelatedToByUppers = append(lowersCorrelatedToByUppers, a)
		}

		// Classify predicates.
		var lowerPredicates []predicates.QueryPredicate
		var upperPredicates []predicates.QueryPredicate
		var deeplyCorrelatedPredicates []predicates.QueryPredicate

		for _, pred := range allPredicates {
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
					// lower flows a source-anchored join RC (qualified ALIAS.COL keys for
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
		// Without this a lower flowing a source-anchored join RC would list duplicate
		// aliases.
		lowersCorrelatedToByUppers = dedupAliases(lowersCorrelatedToByUppers)

		// Skip a disconnected lower: ≥2 quantifiers no predicate links (a pure
		// cross product, e.g. {A,C} for chain A—B—C or {xx,yy} for a star). Its
		// tables share no join, so the partition is a genuine cartesian product
		// — never the cost-optimal shape, and the connected associativities
		// cover the same join orders. Go-tighter than Java (Java explores such
		// lowers and lets cost pruning kill them — dropping this guard blows
		// the 4-chain task baseline past the MaxTasks budget), so it must
		// never eat a bipartition Java NEEDS:
		//   - connectivity is judged by ANY select predicate touching both
		//     aliases — a spanning N-ary predicate (`a.x = b.y OR a.x = c.y`)
		//     connects its aliases even though it can only ever live UPPER
		//     (judging by lowerPredicates alone starved that select of every
		//     bipartition → 0AF00); binary equijoins behave identically under
		//     both readings, so the pinned task baselines are unchanged;
		//   - a COMPONENT-ALIGNED split (isCrossProduct — no component
		//     straddles the halves) whose lower unions SINGLETON components is
		//     exempt: those are the UNAVOIDABLE cross products (`FROM a, b,
		//     c`, the EXISTS body `SELECT 1 FROM t2, t3, t4 …`), the only
		//     bipartitions such a select has. BOTH restrictions are
		//     load-bearing: a component-straddling lower ({d,PB} for
		//     `FROM (derived) d, PB, PB.ARR AS X`) tears a lateral unnest from
		//     its array source, and — on the NAME-MODEL residual — a lower
		//     containing a MULTI-alias component glued only by quantifier
		//     correlation ({PB,X} itself — no predicate links an unnest to its
		//     source) births plans whose AS/AT columns silently NULL.
		//   - RFC-173 W5 (the banked revisit): for an ORDINAL parent (the
		//     gathered flat unnest seed — every parent, now that the anchored
		//     producer is deleted), a quantifier-level correlation edge IS
		//     connectivity: a lower of {source, Explode} is exactly the correct
		//     FlatMap pairing (the W4c binary shape), and rejecting it left the
		//     flat (N+1)-way select unimplementable.
		lowerConnected := aliasesConnectedByPredicates(lowerAliases, allPredicates)
		if !lowerConnected {
			lowerConnected = aliasesConnectedByPredicatesOrCorrelation(lowerAliases, allPredicates, fullCorrelationOrder)
		}
		disconnectedLower := len(lowerAliases) >= 2 && !lowerConnected
		if disconnectedLower &&
			!(isCrossProduct(independentPartitioning, lowerAliases, upperAliases) &&
				lowerComponentsAreSingletons(independentPartitioning, lowerAliases)) {
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

		// The upper select flows the parent result value UNCHANGED. The
		// name-model "merge intent" re-stamp (NewReEnumerationAnchoredRecord over
		// the upper's immediate quantifiers) is DELETED with its producer
		// (RFC-173 S4 item B): no plan carries a source-anchored RC anymore, so
		// there is no hidden-projection merge value to re-anchor — an ordinal
		// seed names exactly the aliases it references, and the positional merge
		// arm (positionalMergeCase) owns the ≥2-live-lowers collapse.

		// addUpper appends the new lower quantifier, the upper tables, and the
		// (rebased) upper predicates to a fresh upper builder.
		//
		// In the MERGE case the new lower quantifier collapses ≥2 live lower
		// tables into ONE quantifier ($m) whose row flows their columns under
		// qualified ALIAS.COL keys. A spanning upper predicate that named such a
		// collapsed table by its bare QOV would reference a correlation the upper
		// select no longer binds: that select would be an INVALID memo member (a
		// predicate over an unbound alias), and a later re-partition would
		// mis-classify it (its correlationTo names a buried table) and sink it
		// into a half that cannot resolve the alias → silent NULL → wrong rows
		// (the root-cause bug). So each such reference is REBASED to read the
		// column through the merge quantifier by qualified name, exactly as the
		// merge result value flows it (the source-anchored join RC keys every live table as
		// ALIAS.COL). After rebasing the predicate's correlation set names the
		// merge alias, which the upper binds — valid AND re-partition-classifiable.
		//
		// In case 1 / case 2 the lower keeps each live table under its ORIGINAL
		// alias (case 2 flows the single live table's row unchanged), so a
		// predicate referencing it resolves directly — collapsedAliases is empty
		// and the predicates pass through unchanged. (RFC-043.)
		addUpper := func(
			newLowerQ expressions.Quantifier,
			collapsedAliases map[values.CorrelationIdentifier]struct{},
		) *GraphExpansionBuilder {
			upperBuilder := NewGraphExpansionBuilder()
			upperBuilder.AddQuantifier(newLowerQ)
			for _, a := range allAliases {
				if _, inUpper := upperAliases[a]; inUpper {
					upperBuilder.AddQuantifier(aliasToQ[a])
				}
			}
			mergeAlias := newLowerQ.GetAlias()
			for _, p := range upperPredicates {
				upperBuilder.AddPredicate(rebaseBuriedLowerReferences(p, collapsedAliases, mergeAlias))
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
				call.MemoizeExpression(applyExistentialSourceAliases(lowerSelectExpr)),
			)
			// No upper-to-lower correlation here, so no spanning predicate
			// references a buried lower — nothing to rebase.
			upperSelectExpression = addUpper(newLowerQ, nil).Build().Seal().
				BuildSelectWithResultValue(resultValue)

		} else if len(lowersCorrelatedToByUppers) >= 2 {
			// Merge case: ≥2 live lower tables. The POSITIONAL merge arm is the
			// sole owner (the name-model ANCHORED re-enumeration arm — the
			// per-alias NewReEnumerationAnchoredRecord lower + the re-stamped
			// upper — was DELETED with its producer, RFC-173 S4 item B; the
			// Slice-3 dispatch-authority pin that an ordinal corpus never
			// reaches the anchored arm is now structural).
			upperSelectExpression = r.positionalMergeCase(call, sel, resultValue, aliasToQ, allAliases, upperAliases, lowersCorrelatedToByUppers, lowerBuilder, upperPredicates)
			if upperSelectExpression == nil {
				continue
			}
			call.Yield(applyExistentialSourceAliases(upperSelectExpression))
			continue
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
				call.MemoizeExpression(applyExistentialSourceAliases(lowerSelectExpr)),
			)
			// The lower flows its single live table's row UNDER ITS ORIGINAL
			// ALIAS, so an upper predicate referencing that alias still resolves
			// directly — nothing buried under a new name, nothing to rebase.
			// (Every lower alias a spanning predicate touches is added to the live
			// set, so with exactly one live lower the only lower alias an upper
			// predicate can name is this one.)
			upperSelectExpression = addUpper(newLowerQ, nil).Build().Seal().
				BuildSelectWithResultValue(resultValue)
		}

		call.Yield(applyExistentialSourceAliases(upperSelectExpression))
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
	// Quantifier.GetCorrelatedTo() returns the empty set in Go (registered
	// divergence, DIVERGENCES.md): in the flat canonical select this term
	// is empty anyway — join predicates live on the Select, not inside the
	// legs — and buried merge-leg deps are re-exposed separately via
	// quantifierMergeSeedLegDeps (RFC-142).
	directDeps := make(map[values.CorrelationIdentifier]map[values.CorrelationIdentifier]struct{}, len(quantifiers))
	for _, q := range quantifiers {
		deps := make(map[values.CorrelationIdentifier]struct{})
		for dep := range q.GetCorrelatedTo() {
			if _, ok := owned[dep]; ok {
				deps[dep] = struct{}{}
			}
		}
		// Partition-time RE-EXPOSURE of a quantifier's BURIED merge-leg deps: a
		// quantifier reading a NON-flow leg's column through a merged row (a
		// multi-source lateral UNNEST's Explode reading `QOV(flowLeg)["SRC.COL"]`)
		// genuinely depends on SRC, but GetCorrelatedTo reports only the flow leg.
		// Add the buried legs so a bipartition cannot separate the Explode from the
		// source its array lives in (which would explode a bare flow-leg row where
		// the array key is unbound — zero rows). RFC-142.
		for leg := range quantifierMergeSeedLegDeps(q) {
			if _, ok := owned[leg]; ok {
				deps[leg] = struct{}{}
			}
		}
		// The quantifier's OWN expression-level correlations to sibling
		// quantifiers — Java's Quantifier.getCorrelatedTo() delegates to
		// rangesOver.getCorrelatedTo(), which Go's quantifier does not
		// (registered divergence); the transitive Reference walk IS
		// implemented, so recover the edges here. A plain lateral UNNEST's
		// Explode correlates to its array source with NO predicate between
		// them: without this edge the source and the unnest are separate
		// "independent components", and the cross-product paths above happily
		// bipartition between them — plans whose AS/AT columns silently NULL.
		// Ordinary table quantifiers have no expression correlations; sibling
		// edges appear exactly where Java sees them.
		if ref := q.GetRangesOver(); ref != nil {
			for dep := range ref.GetCorrelatedTo() {
				if _, ok := owned[dep]; ok && dep != q.GetAlias() {
					deps[dep] = struct{}{}
				}
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

// quantifierMergeSeedLegDeps returns the BURIED merge-leg source aliases a
// quantifier's range value trees depend on — the partition-time re-exposure of a
// quantifier reading a non-flow leg's column through a merged row (a multi-source
// lateral UNNEST's Explode collection value `QOV(flowLeg)["SRC.COL"]`, where SRC
// is a leg merged into flowLeg's row). GetCorrelatedTo reports only the flow leg,
// so these buried deps are otherwise invisible to the bipartition-validity check;
// adding them prevents separating the Explode from the source its array lives in.
//
// It inspects the Explode collection value (the only correlated leaf shape a
// lateral-unnest quantifier ranges over). Returns a non-nil (possibly empty) map.
// RFC-142, the quantifier-level twin of predicates.AddMergeSeedAliases.
func quantifierMergeSeedLegDeps(q expressions.Quantifier) map[values.CorrelationIdentifier]struct{} {
	out := map[values.CorrelationIdentifier]struct{}{}
	ref := q.GetRangesOver()
	if ref == nil {
		return out
	}
	if explode := getExplodeExpression(ref); explode != nil {
		for leg := range values.MergeSeedLegsOfValue(explode.GetCollectionValue()) {
			out[leg] = struct{}{}
		}
	}
	return out
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

// rebaseBuriedLowerReferences rewrites a spanning upper predicate so that every
// reference to a lower table COLLAPSED INTO THE MERGE QUANTIFIER reads its column
// THROUGH the merge quantifier by qualified name.
//
// The merge quantifier (mergeAlias) flows a source-anchored join RC: every live lower
// table's columns are keyed in its row both bare (COL) and table-qualified
// (ALIAS.COL); already-qualified keys (a column carried up from a nested merge)
// pass through verbatim (the anchored RC's field naming, and the executor's
// mergeRows at execution). So a buried table `T`'s column `c` is reachable as
// mergeRow["T.C"]. A FieldValue{Child: QOV(T), Field: c} that referenced the
// (now-buried) T directly is rewritten to FieldValue{Child: QOV(mergeAlias),
// Field: "T.C"} (uppercased to match the qualified-key form). A Field already
// qualified (contains a '.', i.e. T is itself a nested merge carrying
// already-qualified keys) is kept as-is — the source-anchored join RC propagates dotted
// keys verbatim, so re-qualifying would invent a key the merge never wrote.
//
// buriedAliases is the set of lower QUANTIFIER aliases collapsed into the merge
// (its live set). References to UPPER tables (or to lower tables not in the
// merge) are left untouched. Reuses the generic value/predicate replace
// infrastructure (replacePredicateValues + values.Replace) — no GetText hacks.
// buriedAliases empty ⇒ identity (case 1 / case 2 keep their aliases).
func rebaseBuriedLowerReferences(
	p predicates.QueryPredicate,
	buriedAliases map[values.CorrelationIdentifier]struct{},
	mergeAlias values.CorrelationIdentifier,
) predicates.QueryPredicate {
	if len(buriedAliases) == 0 {
		return p
	}
	mergeQOV := values.NewQuantifiedObjectValue(mergeAlias)
	return replacePredicateValues(p, func(v values.Value) values.Value {
		fv, ok := v.(*values.FieldValue)
		if !ok {
			return v
		}
		qov, ok := fv.Child.(*values.QuantifiedObjectValue)
		if !ok {
			return v
		}
		if _, buried := buriedAliases[qov.Correlation]; !buried {
			return v
		}
		// RFC-173 Slice 2 drift assert (same treatment as SelectMergeRule's,
		// contract ruling #1): re-anchoring to a dotted merge name silently
		// degrades a BAKED node to the lazy name model over a re-typed child —
		// exactly the transformation the eager bake exists to forbid. The
		// cluster-arity gate keeps ordinal values out of N-way partition
		// re-stamping entirely; reaching here with a PINNED baked node means
		// the gate mis-scoped (unpinned wrap nodes are childless and never
		// reach this arm; the assert polices the gate's frontier, so it keys
		// on the FrontierPinned contract bit). Loud, never a silent
		// degradation.
		if fv.Resolved != nil && fv.Resolved.FrontierPinned {
			panic(fmt.Sprintf(
				"RFC-173: PartitionSelectRule re-stamp would re-anchor BAKED FieldValue %s#%d (over %s) to merge alias %s — the cluster-arity gate mis-scoped an ordinal join into an N-way re-enumeration (planner bug)",
				fv.Field, fv.Resolved.Root().Ordinal, qov.Correlation.Name(), mergeAlias.Name()))
		}
		field := fv.Field
		if !strings.Contains(field, ".") {
			field = strings.ToUpper(qov.Correlation.Name()) + "." + strings.ToUpper(field)
		}
		return values.NewFieldValue(mergeQOV, field, fv.Typ)
	})
}

var _ ExpressionRule = (*PartitionSelectRule)(nil)

// dedupAliases returns aliases with duplicates removed, sorted by name into a
// CANONICAL order. The live set (lowersCorrelatedToByUppers) is collected from
// map iteration (non-deterministic order), so it must be canonicalized for two
// reasons: (1) determinism — the source-anchored join RC built from it would otherwise
// vary across runs, producing non-deterministic plans; (2) memoization — two
// partitions yielding the same live set must intern to the SAME source-anchored join RC
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

// aliasesConnectedByPredicates reports whether the alias set forms a single
// connected component, where ANY predicate whose correlation set touches two
// of the aliases connects them (union-find). Judged over the select's FULL
// conjunct list, not just the predicates that land in the lower half: a
// spanning N-ary predicate (`a.x = b.y OR a.x = c.y`) genuinely JOINS the
// tables it touches even though it can only ever be evaluated upper — judging
// by lower-resident predicates alone declared every bipartition of such a
// select disconnected and left it unimplementable. For binary equijoins the
// two readings coincide (a predicate inside the lower touches exactly the
// same pair either way), so the join re-enumeration task baselines are
// unchanged.
func aliasesConnectedByPredicates(
	aliases map[values.CorrelationIdentifier]struct{},
	preds []predicates.QueryPredicate,
) bool {
	return aliasesConnectedByPredicatesOrCorrelation(aliases, preds, nil)
}

// aliasesConnectedByPredicatesOrCorrelation is the RFC-173 W5 connectivity
// reading for ORDINAL parents: the union graph of predicate edges AND
// quantifier-level correlation edges (fullCorrelationOrder — an Explode's
// genuine dependency on its array source, the edge Java's
// Quantifier.getCorrelatedTo carries). Plain table quantifiers have no
// correlation edges, so with a nil correlationOrder this IS
// aliasesConnectedByPredicates (which delegates here); the widening admits
// only the unnest-with-source pairings the flat gathered seed relies on.
func aliasesConnectedByPredicatesOrCorrelation(
	aliases map[values.CorrelationIdentifier]struct{},
	preds []predicates.QueryPredicate,
	correlationOrder map[values.CorrelationIdentifier]map[values.CorrelationIdentifier]struct{},
) bool {
	if len(aliases) <= 1 {
		return true
	}
	parent := make(map[values.CorrelationIdentifier]values.CorrelationIdentifier, len(aliases))
	for a := range aliases {
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
	for _, p := range preds {
		var prev values.CorrelationIdentifier
		have := false
		for a := range intersectAliases(aliases, predicates.GetCorrelatedToOfPredicate(p)) {
			if have {
				parent[find(prev)] = find(a)
			}
			prev = a
			have = true
		}
	}
	for a := range aliases {
		for dep := range correlationOrder[a] {
			if _, in := aliases[dep]; in {
				parent[find(a)] = find(dep)
			}
		}
	}
	var root values.CorrelationIdentifier
	first := true
	for a := range aliases {
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

// lowerComponentsAreSingletons reports whether every independent component
// intersecting the lower alias set is a SINGLETON — the pure-cross exemption's
// second gate: a multi-alias component inside a disconnected lower is glued by
// quantifier correlation, not predicates (a lateral unnest and its source),
// and such lowers birth plans the name-model unnest machinery cannot evaluate
// (W5). Singleton components are plain unjoined tables — the genuine
// unavoidable cross product.
func lowerComponentsAreSingletons(
	partitioning []map[values.CorrelationIdentifier]struct{},
	lowerAliases map[values.CorrelationIdentifier]struct{},
) bool {
	for _, comp := range partitioning {
		if len(comp) == 1 {
			continue
		}
		for a := range comp {
			if _, inLower := lowerAliases[a]; inLower {
				return false
			}
		}
	}
	return true
}
