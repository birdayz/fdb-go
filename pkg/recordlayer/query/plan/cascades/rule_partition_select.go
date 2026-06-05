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
		//   - A translator SEED merge (an anchored join RC, isAnchoredJoinResult)
		//     names only its two immediate source legs but HIDES the real projection
		//     (which lives in the Project above): SelectMergeRule flattens a
		//     pre-flatten binary sub-join's quantifiers up without rewriting the
		//     parent merge, so the anchored RC's named set is untrustworthy.
		//     Conservatively keep ALL lower aliases live.
		//   - A re-enumeration merge lists EXACTLY the aliases its
		//     parent needs (GetCorrelatedToOfValue returns them). Keep only those —
		//     flowing all would generate far more distinct merge sub-products than
		//     needed, blowing up the search space.
		//   - Any other result value (a bare projection) marks live only the lowers
		//     it actually references.
		// A source-anchored join RESULT value (RFC-077 7.6) HIDES the real
		// projection: its fields name only the immediate source legs (the Project
		// lives above), so the rule cannot trust its named set and must keep ALL
		// lower aliases live. GetCorrelatedToOfValue deliberately hides the RC's leg
		// QOVs (exploration-time budget), so the else-branch would see an empty set
		// and drop every buried column — this branch re-exposes them. (This is the
		// structural successor of the retired opaque-seed provenance bit, whose
		// alias-boundness heuristic silently dropped buried-table columns — the
		// 4-way regression the FDB N-way test caught.)
		_, anchoredSeed := isAnchoredJoinResult(resultValue)
		if anchoredSeed {
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
			// Augment the correlation set with the anchored join RC's source leg
			// aliases (GetCorrelatedToOfPredicate hides them — see
			// AddMergeSeedAliases). Without this, a predicate reading a buried
			// table's column through a seed merge (e.g. FieldValue(QOV($m), "B.AID"))
			// reports only its non-merge side, so a predicate spanning both
			// partition halves is misclassified as lower-only and pushed below
			// the merge to a leaf scan where the buried alias is unbound — the
			// 0-row dual-correlation join (TestFDB_JoinMerge_OuterColumn_NotDropped).
			correlatedTo := predicates.GetCorrelatedToOfPredicate(pred)
			predicates.AddMergeSeedAliases(pred, correlatedTo)
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

		// The upper select must flow a result value over the aliases it ACTUALLY
		// binds (the new lower quantifier + the upper tables). The merge result
		// value (the sole source-anchored join RC — flattened seed or intermediate
		// re-enumeration) carries "flow all my tables' columns merged" intent, but
		// names the ORIGINAL deep aliases. When a
		// partition collapses ≥2 of those tables into one merge quantifier ($m),
		// those original aliases are NO LONGER bound at the upper level: they live
		// inside $m's merged row under qualified ALIAS.COL keys. Flowing the original
		// merge value unchanged would then look up correlations the upper never binds
		// → every column resolves to nil → wrong rows (a two-level merge returning
		// NULL for a deeply-nested projected column — the root-cause bug).
		//
		// So whenever the parent result is a merge value, re-stamp it as a
		// source-anchored join RC (NewReEnumerationAnchoredRecord) over the upper's
		// IMMEDIATE quantifiers. The new lower
		// quantifier's merged row preserves every collapsed table's qualified keys
		// verbatim (the executor's mergeRows passes dotted keys
		// through), so the re-stamped upper merge accumulates all live columns, and a
		// projection/predicate above resolves each table's column from the final
		// merged row by qualified name. This is correct at the top level too: the
		// re-stamped anchored RC is the output the Project consumes, and the
		// Project's FieldValues resolve the qualified keys the merge flows. In the
		// non-collapsing branches (case 1 / case 2 the new lower keeps its single
		// table's original alias) the re-stamp is a no-op in effect — the alias is
		// still bound — but covering all branches keeps the result value consistent
		// with the aliases the upper binds. (RFC-043.)
		// The parent result is a "merge intent" when it is the source-anchored join
		// RESULT value (RFC-077 7.6): it means "flow all my tables' columns merged,
		// named by the original deep aliases." When a partition collapses ≥2 of those
		// tables into one merge quantifier, those aliases are no longer bound at the
		// upper level, so the parent value must be RE-STAMPED as a re-enumeration
		// source-anchored join RC over the upper's IMMEDIATE quantifiers
		// (NewReEnumerationAnchoredRecord re-anchors the parent's columns to the new
		// legs). Every real-table parent reaching here is already anchored — the
		// retired opaque restamp is gone.
		parentAnchored, parentIsMerge := isAnchoredJoinResult(resultValue)
		// upperLegs returns the canonical (alias-name-sorted) leg order for a
		// re-enumeration result over [newLowerAlias] + the upper tables.
		upperLegs := func(newLowerAlias values.CorrelationIdentifier, newLowerSources []values.CorrelationIdentifier) []values.ReEnumerationLeg {
			legs := make([]values.ReEnumerationLeg, 0, len(upperAliases)+1)
			legs = append(legs, values.ReEnumerationLeg{Alias: newLowerAlias, Sources: newLowerSources})
			for _, a := range allAliases {
				if _, inUpper := upperAliases[a]; inUpper {
					legs = append(legs, values.ReEnumerationLeg{Alias: a, Sources: []values.CorrelationIdentifier{a}})
				}
			}
			sort.Slice(legs, func(i, j int) bool { return legs[i].Alias.Name() < legs[j].Alias.Name() })
			return legs
		}
		// buildUpperResult re-stamps the parent merge over the upper's immediate
		// quantifiers. newLowerSources is the set of parent quantifiers the new lower
		// quantifier's row carries (empty for a cross-product literal-1 lower,
		// {newLowerAlias} for a single table, the collapsed set for a merge $m).
		//
		// RFC-077 7.6 re-enumeration anchoring: when the parent is a source-anchored
		// RC, build a NEW source-anchored RC over the upper's quantifiers
		// (NewReEnumerationAnchoredRecord, columns read from the parent by anchoring
		// quantifier). The retired opaque merge restamp is gone — every
		// real-table parent reaching here is anchored (proven by the no-fallback
		// sentinels + the full-suite panic probe), and NewReEnumerationAnchoredRecord
		// always resolves an anchored parent's columns (every leg's source is a parent
		// quantifier). Canonical leg order so two bipartitions producing the same upper
		// merge intern (the anchored RC's structural identity is order-sensitive).
		buildUpperResult := func(newLowerAlias values.CorrelationIdentifier, newLowerSources []values.CorrelationIdentifier) values.Value {
			if !parentIsMerge {
				return resultValue
			}
			legs := upperLegs(newLowerAlias, newLowerSources)
			return values.NewReEnumerationAnchoredRecord(parentAnchored, legs)
		}

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
				call.MemoizeExpression(lowerSelectExpr),
			)
			// No upper-to-lower correlation here, so no spanning predicate
			// references a buried lower — nothing to rebase.
			upperSelectExpression = addUpper(newLowerQ, nil).Build().Seal().
				BuildSelectWithResultValue(buildUpperResult(lowerAliasCorrelatedToByUpperAliases, nil))

		} else if len(lowersCorrelatedToByUppers) >= 2 {
			// Merge case: ≥2 live lower tables (referenced by an upper predicate or
			// the result value). The lower flows a source-anchored join RC carrying
			// qualified ALIAS.COL keys for every live table; the upper predicates
			// and result value resolve those columns through the merged row by
			// table-qualified name — no translation. A chain of such merges
			// accumulates all live columns up the join spine (each merge preserves
			// already-qualified keys). Reaching here implies
			// lowersCorrelatedToByUpperAliases == 0 (the validation above skips a
			// quantifier-level upper-dep with >1 live lower). (RFC-043.)
			// RFC-077 7.6: the lower merges the live lower quantifiers' rows. Anchor it
			// over those quantifiers (each a leg whose columns the parent RC supplies).
			// The retired opaque merge is gone — the parent is always anchored
			// here (proven), so NewReEnumerationAnchoredRecord resolves every leg.
			legs := make([]values.ReEnumerationLeg, len(lowersCorrelatedToByUppers))
			for i, a := range lowersCorrelatedToByUppers {
				legs[i] = values.ReEnumerationLeg{Alias: a, Sources: []values.CorrelationIdentifier{a}}
			}
			lowerResult := values.NewReEnumerationAnchoredRecord(parentAnchored, legs)
			lowerSelectExpr := lowerBuilder.Build().Seal().BuildSelectWithResultValue(lowerResult)

			// A per-plan deterministic merge alias (Memo.NextMergeAlias). Two
			// bipartitions that produce the SAME merged sub-join get DIFFERENT
			// aliases but still intern to one memo Reference, because
			// Reference.Insert/InsertFinal dedup ALIAS-AWARE (MemoEqual): upper
			// SelectExpressions equal up to a consistent quantifier-alias renaming
			// collapse to one member, so the re-enumeration's shared sub-products
			// (e.g. the (A⋈B) merge reached from many bipartitions) are NOT
			// re-explored per path. This retires the former synthetic STABLE
			// "$m_<len>:<name>…" alias — a workaround that made the upper selects
			// byte-identical for the previously alias-SENSITIVE Insert; interning is
			// now structural (RFC-074 made the merge value's hash alias-invariant).
			// The alias is per-Memo (per-plan) deterministic, NOT process-global
			// uniqueId, so the same query mints the same alias sequence across
			// plannings → a STABLE plan hash (the merge alias flows into the NLJ
			// source alias and thus PlanHash/plan-log identity + the cost-model
			// tiebreak; a global uniqueId would churn those across a process's
			// history — codex P2). On test/utility firing paths the memo is nil; fall
			// back to a process-unique alias there (no plan-hash stability needed).
			// (RFC-077 7.5.)
			var mergeAlias values.CorrelationIdentifier
			if call.memo != nil {
				mergeAlias = call.memo.NextMergeAlias()
			} else {
				mergeAlias = values.UniqueCorrelationIdentifier()
			}
			newLowerQ := expressions.NamedForEachQuantifier(
				mergeAlias,
				call.MemoizeExpression(lowerSelectExpr),
			)
			// The merge collapses every live lower table into one quantifier
			// keyed by ALIAS.COL. Any upper (spanning) predicate that named a
			// collapsed lower table by its bare alias must be rebased to read
			// that column through the merge quantifier — otherwise it references
			// an alias the upper select no longer binds (the root-cause bug).
			collapsed := make(map[values.CorrelationIdentifier]struct{}, len(lowersCorrelatedToByUppers))
			for _, a := range lowersCorrelatedToByUppers {
				collapsed[a] = struct{}{}
			}
			upperSelectExpression = addUpper(newLowerQ, collapsed).Build().Seal().
				BuildSelectWithResultValue(buildUpperResult(mergeAlias, lowersCorrelatedToByUppers))

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
			// The lower flows its single live table's row UNDER ITS ORIGINAL
			// ALIAS, so an upper predicate referencing that alias still resolves
			// directly — nothing buried under a new name, nothing to rebase.
			// (Every lower alias a spanning predicate touches is added to the live
			// set, so with exactly one live lower the only lower alias an upper
			// predicate can name is this one.)
			upperSelectExpression = addUpper(newLowerQ, nil).Build().Seal().
				BuildSelectWithResultValue(buildUpperResult(lowerAlias, []values.CorrelationIdentifier{lowerAlias}))
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
		field := fv.Field
		if !strings.Contains(field, ".") {
			field = strings.ToUpper(qov.Correlation.Name()) + "." + strings.ToUpper(field)
		}
		return values.NewFieldValue(mergeQOV, field, fv.Typ)
	})
}

// isAnchoredJoinResult reports whether v is a source-anchored join RESULT value
// (RFC-077 7.6) — the RecordConstructorValue NewAnchoredJoinRecord builds, marked
// AnchoredJoin. It is the structural successor of the retired opaque merge's Seed bit:
// PartitionSelectRule treats it the same way (keep all lower aliases live; re-stamp
// to a re-enumeration merge when collapsing). Returns the RC for callers that need
// it.
func isAnchoredJoinResult(v values.Value) (*values.RecordConstructorValue, bool) {
	rc, ok := v.(*values.RecordConstructorValue)
	if !ok || !rc.AnchoredJoin {
		return nil, false
	}
	return rc, true
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
