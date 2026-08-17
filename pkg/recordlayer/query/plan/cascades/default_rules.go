package cascades

// DefaultExpressionRules returns the REWRITING-phase exploration rule
// set the optimiser fires by default: uncontroversial
// logical-to-logical rewrites that don't need cost-aware decisions
// (the physical-implementation rules live in BatchAExpressionRules /
// DefaultImplementationRules, PLANNING phase). Order matters within an
// exploration round: merge-rules run first (collapse nested operators
// of the same kind), then no-op-elimination rules run on the merged
// shapes. Order across rounds doesn't matter because the task-stack
// driver re-fires every rule against newly-yielded members until the
// group saturates.
//
// **Alias rebasing caveat**: every Push*Through* and Pull*Above* rule
// in this set moves a Filter (or TypeFilter) across a Quantifier
// boundary without rewriting the moved operator's internal alias
// references — predicates and Values inside the moved operator keep
// referencing the ORIGINAL quantifier's alias (no TranslationMap
// rebase at the move site). Harmless for row-set semantics (no
// evaluator resolves the moved predicates through those aliases), but
// a rule that inspects correlation structure
// (GetCorrelatedToWithoutChildren) on a pushed predicate sees the
// stale alias. Correlation-aware logic must not consume pushed/pulled
// predicates without a TranslationMap rebase (translation_map.go)
// first.
//
// **ProjectionMergeRule's soundness contract**: the merge
// `Projection(P1) over Projection(P2) over X → Projection(P1) over X`
// is sound only because LogicalProjectionExpression's GetResultValue()
// passes the inner row through (projection is a pure side channel).
// If projection ever narrows the row shape (materialized projections),
// P1's Values may reference computed columns that only exist in P2's
// output — at that point the rule needs a column-substitution rewrite
// path or it must leave the default set. See ProjectionMergeRule's
// own doc for the per-rule discussion.
//
// Each call returns a fresh slice — callers may mutate freely. Each
// element is a fresh rule instance — see NewXxxRule constructors for
// the per-call allocation contract.
func DefaultExpressionRules() []ExpressionRule {
	return []ExpressionRule{
		NewFilterMergeRule(),
		NewFilterDropTruePredicatesRule(),
		NewFilterDedupPredicatesRule(),
		NewPushFilterThroughDistinctRule(),
		NewPushFilterThroughTypeFilterRule(),
		NewPushFilterThroughGroupByRule(),
		NewPushFilterBelowJoinRule(),
		NewDistinctMergeRule(),
		NewDistinctOverSortElimRule(),
		NewDistinctOverUnionDedupRule(),
		NewDistinctOverGroupByElimRule(),
		// DistinctOnUniqueElimRule REMOVED (D-3): Java's ImplementDistinctRule
		// is PLANNING-phase only. Distinct elimination now happens exclusively
		// in ImplementDistinctFinalRule (DefaultImplementationRules).
		NewPullFilterAboveDistinctRule(),
		NewTypeFilterMergeRule(),
		NewTypeFilterRedundantOverScanRule(),
		NewPushTypeFilterBelowFilterRule(),
		NewUnionMergeRule(),
		NewIntersectionMergeRule(),
		// PushFilterThroughUnion/Intersection + PullCommonFilterAbove
		// Union/Intersection REMOVED (RFC-185, Go-only). The inverse pairs
		// interned into a cyclic memo (GetCorrelatedTo stack overflow); they
		// never yielded on the corpus and Java reaches these plans via
		// match-then-implement data access, not logical commutation. Do NOT
		// re-add: a filter/set-op commutation rule needs a cycle-safe memo
		// design first (guarding the transitive intern-to-ancestor makes the
		// inverse-pair fixpoint non-terminate — proven, see RFC-185).
		NewNoOpFilterRule(),
		NewProjectionMergeRule(),
		// PushProjectionBelowJoinRule REMOVED (Go-only, no Java equivalent).
		// It wrapped a join's children in LogicalProjectionExpressions, which
		// blocked SelectMergeRule from flattening the nested binary join into
		// the canonical flat N-quantifier SelectExpression — so PartitionSelectRule
		// had no flat seed to re-enumerate join associativities from, locking the
		// plan to FROM-clause order (RFC-042 L1). Its only load-bearing use was
		// temp-table column alignment in a recursive-CTE body, now handled at
		// translation time (the recursive leg's normalization projection emits
		// clean seed-schema rows — cascades_translator.go), so the rule is gone.
		// PullFilterAboveSortRule REMOVED: Go-only rule not in Java.
		// Pulling Filter above Sort changes the correlation structure and
		// caused InJoin to wrap Sort inside it, then InJoin.HintOrdering
		// falsely claimed ordering → sort eliminated → wrong results.
		// PushFilterThroughSortRule REMOVED: Go-only rule not in Java.
		// Same class of issue as PullFilterAboveSortRule.
		// PullFilterAboveProjectionRule REMOVED: Go-only rule not in Java.
		// Pulling Filter above Projection put Filter where it couldn't
		// find projected columns, causing resolution failures.
		// PushFilterThroughProjectionRule REMOVED: Go-only rule not in Java.
		// Same class of issue as PullFilterAboveProjectionRule.
		NewSortMergeRule(),
		NewSortDedupKeysRule(),
		NewSortConstantKeysElimRule(),
		NewUnsortedSortElimRule(),
		// PushOrderingThroughGroupByRule REMOVED (D-2): moved to PLANNING
		// phase as PushRequestedOrderingThroughGroupByRule (DefaultImplementationRules).
		// PushOrderingThroughProjectionRule REMOVED: moved to PLANNING
		// phase as PushRequestedOrderingThroughProjectionRule (DefaultImplementationRules).
		// PushOrderingThroughFilterRule REMOVED (D-3): moved to PLANNING
		// phase as PushRequestedOrderingThroughFilterRule (DefaultImplementationRules).
		// PushOrderingThroughDistinctRule REMOVED (D-2): moved to PLANNING
		// phase as PushRequestedOrderingThroughDistinctRule (DefaultImplementationRules).
		// PushOrderingThroughUniqueRule REMOVED (D-2): moved to PLANNING
		// phase as PushRequestedOrderingThroughUniqueRule (DefaultImplementationRules).
		// PushOrderingThroughUnionRule REMOVED (D-2): moved to PLANNING
		// phase as PushRequestedOrderingThroughUnionRule (DefaultImplementationRules).
		// PushOrderingThroughDeleteRule REMOVED (D-2): moved to PLANNING
		// phase as PushRequestedOrderingThroughDeleteRule (DefaultImplementationRules).
		// PushOrderingThroughInsertRule REMOVED: moved to PLANNING phase
		// as PushRequestedOrderingThroughInsertRule (DefaultImplementationRules).
		// PushOrderingThroughUpdateRule REMOVED: moved to PLANNING phase
		// as PushRequestedOrderingThroughUpdateRule (DefaultImplementationRules).
		// PushOrderingThroughTempTableInsertRule REMOVED: moved to PLANNING
		// phase as PushRequestedOrderingThroughTempTableInsertRule (DefaultImplementationRules).
		NewUnionSingletonElimRule(),
		NewIntersectionSingletonElimRule(),
		NewInComparisonToExplodeRule(),
		NewLimitMergeRule(),
		// PushLimitThroughProjectionRule REMOVED (Go-only, no Java equivalent).
		// Java expresses a row limit as ExecuteProperties.setReturnedRowLimit()
		// at EXECUTION and has no limit-pushing planner rule at all — 0 of the
		// 70 .java files under query/plan/cascades/rules name limit or vector.
		//
		// It rewrote Limit(Project(X)) into Project(Limit(X)) during REWRITING,
		// where OptimizeGroupTask's partition-retention block does not run. So
		// REWRITING pruned to the single pushed survivor and the un-pushed
		// shape — whose inner group holds the covering winner — never reached
		// the phase that offers the covering rewrite. A FETCHING index scan won
		// where a covering one existed, and the cost model was never consulted:
		// the better member was ABSENT, not outranked. Pinned by
		// embedded.TestLimitOverProjectionKeepsTheCoveringRewrite.
		//
		// Nothing downstream needed the push, which is the part worth stating
		// because it is not obvious. The one rule that wants a Limit DIRECTLY
		// above its scan is SinkLimitIntoVectorScanRule (registered in
		// DefaultImplementationRules), and the Limit it folds is the QUALIFY
		// rank cap's — synthesized UNDER the projection already, so the fold
		// fires with a projection present. Pinned by
		// embedded.TestVectorPlan_NoResidualFoldsToSelfLimiting and, for an
		// explicit LIMIT equal to that cap (the only shape where an OUTER limit
		// passes the fold's equality gate), by
		// embedded.TestVectorPlan_ExplicitLimitEqualToRankStillFolds.
		NewPushLimitThroughUnionRule(),
		NewNoOpLimitElimRule(),
		NewSelectMergeRule(),
		NewSplitSelectExtractIndependentQuantifiersRule(),
		NewNormalizePredicatesRule(),
		NewPredicateToLogicalUnionRule(),
		// Join-order enumeration (PartitionSelectRule / PartitionBinarySelectRule)
		// is PLANNING-only — see PlanningExplorationRules, matching Java's
		// PlanningRuleSet (the RewritingRuleSet is normalization only). REWRITING
		// normalizes to the canonical flat N-quantifier SelectExpression
		// (SelectMergeRule); since no partitioning runs here, every member promoted
		// to PLANNING is flat, so PartitionSelectRule re-enumerates all join
		// associativities in PLANNING where the stats-aware PlanningCostModel
		// (RFC-041) picks the cheapest order. Firing them in REWRITING locked the
		// FROM-order associativity at the phase boundary (RFC-042).
		NewDecorrelateValuesRule(),
		NewRemoveRangeOneRule(),
		NewEliminateNullOnEmptyRule(),
		// Index-candidate matching (MatchLeafRule / MatchIntermediateRule) is
		// PLANNING-only — see PlanningExplorationRules, matching Java's
		// PlanningRuleSet (match-then-implement happens in PLANNING). Running it
		// in REWRITING too double-matched references whose absorbed-inner
		// Selects are created in PLANNING, producing duplicate index-scan
		// members (e.g. Intersection of an index scan with itself). REWRITING is
		// normalization only.
	}
}

// PlanningExplorationRules returns the exploration rules that re-fire
// during the PLANNING phase after advancePlannerStage clears EXPLORE
// artifacts. These re-derive logical alternatives from the promoted
// canonical seed. Mirrors Java's PlanningRuleSet.EXPLORATION_RULES.
func PlanningExplorationRules() []ExpressionRule {
	return []ExpressionRule{
		NewNormalizePredicatesRule(),
		NewInComparisonToExplodeRule(),
		NewSplitSelectExtractIndependentQuantifiersRule(),
		NewEliminateNullOnEmptyRule(),
		// Re-fire the LEFT-OUTER canonicalizer in PLANNING so a LEFT OUTER that only
		// surfaces here is still rewritten to the correlated null-supplying form the
		// FlatMap path consumes. This is a Go-only rule (Java's PlanningRuleSet has no
		// RewriteOuterJoinRule) and an INTENTIONAL divergence: unlike Java, Go keeps the
		// un-rewritten outer-join SelectExpression as the REWRITING prune survivor (it is
		// directly implementable as a materialized RecordQueryNestedLoopJoinPlan, RFC-152 —
		// see RewritingCostModelLess), so PLANNING re-derives the rewritten form here to
		// keep the correlated-FlatMap alternative available alongside the materialized NLJ.
		// (Java can drop it because outerJoinCount forces the rewritten form to survive the
		// prune — a guard Go must NOT adopt, or it would suppress the materialized NLJ.)
		NewRewriteOuterJoinRule(),
		NewPartitionSelectRule(),
		NewPartitionBinarySelectRule(),
		// Match candidates (index selection) in PLANNING as well as REWRITING.
		// PartitionBinarySelectRule absorbs a join predicate into a correlated
		// inner Select([join pred], Scan) *during PLANNING*; that inner must be
		// matched against index candidates here (REWRITING never saw it) so its
		// correlated equi-predicate SARGs the index, yielding an index-probe
		// inner — the inner of an index-nested-loop join (RFC-042 L3). Java
		// runs match-then-implement in its PlanningRuleSet; the Go port had
		// matching only in REWRITING, so join inners (PLANNING artifacts) never
		// index-matched and every join full-scanned its inner.
		NewMatchLeafRule(),
		NewMatchIntermediateRule(),
	}
}

// BatchAExpressionRules returns the physical-implementation rules
// (PrimaryScanRule, ImplementFilterRule, etc.). These fire during the
// PLANNING phase via WithPlanningExpressionRules. They yield to
// InsertFinal so their results land in FinalMembers.
//
// Uses Java's PlanningRuleSet.IMPLEMENTATION_RULES as its base. Documented Go
// extensions are included where the read-side architecture differs.
//
// A BARE `UNION ALL` is implemented by ImplementUnorderedUnionRule alone, as in
// Java. Go also had ImplementUnionRule, emitting a second concat plan for the
// SAME logical shape at the SAME cost — the two are both operator-neutral — so
// which one won was decided by exploration order rather than by cost, and it
// flipped on unrelated changes. Java has no such rule (its RecordQueryUnionPlan
// variants require compatible comparison keys and arise from ordered/
// distinct-union planning), and the Go rule produced a keyless concat every
// time. THAT is the reason for the deletion, and it stands on its own.
//
// AN EARLIER VERSION OF THIS NOTE ALSO CLAIMED RecordQueryUnionPlan "is an
// eager concat that DECLINES a continuation", and the executor says otherwise.
// executeUnion routes to executeUnionStreaming whenever every branch's column
// names are statically known — lazy, per-branch CursorFactory, resuming off a
// branch-tagged ConcatContinuation. Only executeUnionBuffered declines, and only
// on the fallback where the names are not statically known. On the eagerness
// axis the deletion in fact moved the OTHER way: the surviving
// executeUnorderedUnion opens every child cursor up front. Keeping the wrong
// reason next to a right conclusion is how a later reader talks themselves into
// reverting the conclusion.
//
// It did serve one shape alone, and only because of a second Go-only
// divergence: ImplementUnorderedUnionRule carried a two-leg floor Java's rule
// does not have, so a ONE-leg logical union that survived
// UnionSingletonElimRule had no implementer. The floor is gone (see that
// rule), which is what makes the unordered rule sufficient on its own.
func BatchAExpressionRules() []ExpressionRule {
	return []ExpressionRule{
		NewPrimaryScanRule(),
		NewImplementValuesRule(),
		NewImplementProjectionRule(),
		NewImplementFilterRule(),
		NewOrderedIndexScanRule(),
		NewOrderedPrimaryScanRule(),
		NewImplementTypeFilterRule(),
		NewImplementIntersectionRule(),
		NewImplementStreamingAggregationRule(),
		NewStreamingAggFromIndexRule(),
		NewAggregateDataAccessRule(),
		NewImplementNestedLoopJoinRule(),
		NewImplementLimitRule(),
		NewImplementTempTableScanRule(),
		NewImplementTempTableInsertRule(),
		NewImplementRecursiveDfsJoinRule(),
		NewImplementRecursiveLevelUnionRule(),
		NewImplementExplodeRule(),
		NewImplementTableFunctionRule(),
	}
}

// DMLImplementationRules returns the DML-side implementation rules
// (ImplementInsertRule, ImplementDeleteRule). Mirrors Java's
// per-DML implementation rule set.
//
// Compose with: append(rules, DMLImplementationRules()...) when
// the planner needs to physical-implement DML expressions
// (INSERT / DELETE / UPDATE).
//
// All 3 DML implement rules now ported (Insert / Delete / Update).
// Per-row transform application for Update happens at execution
// time, not rule-fire time — transforms pass through unchanged.
func DMLImplementationRules() []ExpressionRule {
	return []ExpressionRule{
		NewImplementInsertRule(),
		NewImplementDeleteRule(),
		NewImplementUpdateRule(),
	}
}

// DefaultImplementationRules returns the ImplementationRules for the
// PLANNING phase.
func DefaultImplementationRules() []ImplementationRule {
	rules := []ImplementationRule{
		// --- Constraint-push rules (top-down, PLANNING Phase 1) ---
		// These fire during constraintOnly=true to propagate ordering
		// constraints from parent to child References. Ports Java's
		// PushRequestedOrderingThrough*Rule family.
		NewPushRequestedOrderingThroughSortRule(),
		NewPushRequestedOrderingThroughDistinctRule(),
		NewPushRequestedOrderingThroughUniqueRule(),
		NewPushRequestedOrderingThroughFilterRule(),
		NewPushRequestedOrderingThroughDeleteRule(),
		NewPushRequestedOrderingThroughInsertRule(),
		NewPushRequestedOrderingThroughUpdateRule(),
		NewPushRequestedOrderingThroughTempTableInsertRule(),
		NewPushRequestedOrderingThroughProjectionRule(),
		NewPushRequestedOrderingThroughGroupByRule(),
		NewPushRequestedOrderingThroughUnionRule(),
		NewPushRequestedOrderingThroughRecursiveUnionRule(),
		NewPushRequestedOrderingThroughSelectExistentialRule(),
		NewPushRequestedOrderingThroughSelectRule(),
		NewPushRequestedOrderingThroughInLikeSelectRule(),

		// --- Referenced-field push rules (column pruning, top-down) ---
		NewPushReferencedFieldsThroughFilterRule(),
		NewPushReferencedFieldsThroughSelectRule(),
		NewPushReferencedFieldsThroughDistinctRule(),
		NewPushReferencedFieldsThroughUniqueRule(),

		// --- Java-ported rules (1:1 with fdb-record-layer-core) ---
		NewImplementSimpleSelectRule(),
		NewImplementDistinctUnionRule(),
		NewImplementInJoinRule(),
		NewImplementInUnionRule(),
		NewImplementSortRule(),
		NewImplementProjectionFinalRule(),
		NewImplementDistinctFinalRule(),
		NewImplementUniqueRule(),
		NewImplementUnorderedUnionRule(),

		// --- Vector scan limit fold (RFC-156 Phase B) ---
		// Folds a Limit(k) directly above an ordered-stream VectorIndexScan back
		// into the scan's self-limiting top-k mode (restores the legacy one-shot
		// search(k) for no-residual / partition-only queries). Does NOT fire when
		// a residual Filter intervenes — those keep the Limit→Filter→ordered-scan
		// form that returns the true k nearest MATCHING rows.
		NewSinkLimitIntoVectorScanRule(),

		// --- Fetch push-through rules (physical plan optimization) ---
		//
		// MergeFetchIntoCoveringIndexRule collapses Fetch(Covering(Index)) into a
		// bare Index — semantically identical (a bare index plan resolves its own
		// records by primary key) and one node cheaper. Java registers it
		// (PlanningRuleSet.java:151) and so does Go: it is a sound transformation
		// and the competition it creates is the same competition Java has.
		//
		// Registering it currently COSTS RFC-220's target plan, and the reason is
		// not this rule. MEASURED on the defect query: the fetch-rooted member is
		// NOT deleted — the group MergeFetchIntoCoveringIndexRule yields into holds
		// [FETCH, INDEX] together afterwards, since InsertFinal appends. But the
		// group PushFilterThroughFetchRule scans is a DIFFERENT reference holding
		// [INDEX] alone, so the rule never finds a fetch and the
		// Fetch(Filter(Covering)) alternative is never built. The costing is not at
		// fault either: on the two final trees (both 3 nodes) criterion 7 returns
		// -1 and the full ladder prefers the covering tree.
		//
		// So the open defect is a reference-IDENTITY question — which group the
		// filter ends up ranging over — not a prune, not a cost comparison, and not
		// this rule's soundness. It stays registered because Java registers it and
		// it is a sound transformation; the identity question is tracked as RFC-220's
		// remaining blocker.
		NewMergeFetchIntoCoveringIndexRule(),
		NewPushDistinctBelowFilterRule(),
		NewPushDistinctThroughFetchRule(),
		NewPushFilterThroughFetchRule(),
		NewPushMapThroughFetchRule(),
		NewPushInJoinThroughFetchRule(),
		NewPushUnionThroughFetchRule(),
		NewPushIntersectionThroughFetchRule(),
		NewPushUnorderedUnionThroughFetchRule(),
		NewPushMergeSortUnionThroughFetchRule(),
		NewPushInUnionThroughFetchRule(),
		NewRemoveProjectionRule(),
		NewMergeProjectionAndFetchRule(),
	}

	rules = append(rules, GoExtensionImplementationRules()...)
	return rules
}

// GoExtensionImplementationRules returns implementation rules that have
// no Java equivalent. These extend the Cascades planner with in-memory
// post-processing operators (RFC-001). Registered separately so the
// boundary between Java-ported and Go-extension rules is explicit.
func GoExtensionImplementationRules() []ImplementationRule {
	return []ImplementationRule{
		NewImplementInMemorySortRule(),
	}
}

// RewritingRules returns the exploration rules for the REWRITING phase.
// Mirrors Java's RewritingRuleSet.EXPLORATION_RULES:
//   - QueryPredicateSimplificationRule: constant-fold predicate values
//   - PredicatePushDownRule: push predicates into child quantifiers
//   - DecorrelateValuesRule: inline constant value boxes
//
// These rules are NOT part of DefaultExpressionRules — they target the
// REWRITING phase, which runs before the main PLANNING phase to
// normalise the expression tree. The planner driver composes rule sets
// as needed.
func RewritingRules() []ExpressionRule {
	return []ExpressionRule{
		NewQueryPredicateSimplificationRule(),
		NewPredicatePushDownRule(),
		NewDecorrelateValuesRule(),
		// Canonicalize LEFT OUTER joins away before planning (Java's
		// RewritingRuleSet runs RewriteOuterJoinRule): push ON-predicates below the
		// null-extension boundary into a correlated null-supplying SUBSEL so the
		// data-access FlatMap path can plan a correlated LEFT-OUTER join.
		NewRewriteOuterJoinRule(),
	}
}

// OptionalRewritingRuleNames returns the names of the rules Java's
// `RecordQueryPlannerConfiguration.Builder.disableRewritingRules()` turns
// off — `RewritingRuleSet.OPTIONAL_RULES`, i.e. every rule of the REWRITING
// rule set EXCEPT `FinalizeExpressionsRule`, which stays on because it is
// what stamps an expression FINAL and planning cannot proceed without it.
//
// Java's set is {QueryPredicateSimplificationRule, PredicatePushDownRule,
// DecorrelateValuesRule, RewriteOuterJoinRule, SelectMergeRule}; all five
// exist in Go under the same names, so the disabled set is name-identical.
// SelectMergeRule is in Go's DefaultExpressionRules rather than
// RewritingRules (Go's REWRITING phase runs the default expression rules),
// so it is named explicitly instead of being derived from RewritingRules().
//
// The names are the Java simple-class-name spelling, which is exactly what
// Planner.DisabledRules is keyed by.
func OptionalRewritingRuleNames() []string {
	rewriting := RewritingRules()
	names := make([]string, 0, len(rewriting)+1)
	for _, r := range rewriting {
		names = append(names, shortTypeName(r))
	}
	return append(names, shortTypeName(NewSelectMergeRule()))
}

// MatchingRules returns the matching rules that seed the partial-match
// infrastructure. These fire during the EXPLORE phase alongside
// expression rules but serve a different purpose: they establish
// PartialMatch instances between the query graph and MatchCandidate
// traversals, which are then consumed by implementation rules to
// produce index-scan plans.
//
// Mirrors Java's PlanningRuleSet.MATCHING_RULES:
//   - MatchLeafRule: seeds leaf-to-leaf matches
//   - MatchIntermediateRule: composes child matches into parent matches
//
// Compose with: append(DefaultExpressionRules(), MatchingRules()...)
func MatchingRules() []ExpressionRule {
	return []ExpressionRule{
		NewMatchLeafRule(),
		NewMatchIntermediateRule(),
	}
}

// init registers the default rules in the rule registry under their
// concrete-type names ("FilterMergeRule", etc.) — discoverable via
// LookupRule / RegisteredRuleNames for diagnostic output.
//
// One registry entry per rule TYPE; the actual rule instances in
// DefaultExpressionRules() are fresh per-call (rules are stateless).
func init() {
	registerDefaultRules()
	registerBatchARules()
	registerMatchingRules()
	registerRewritingRules()
}

func registerRewritingRules() {
	for _, r := range RewritingRules() {
		name := shortTypeName(r)
		if LookupRule(name) != nil {
			continue
		}
		RegisterRule(name, r)
	}
}

func registerDefaultRules() {
	for _, r := range DefaultExpressionRules() {
		// Use the concrete type name (without leading * and package
		// prefix) as the registry key. Skip if already registered —
		// init can be called twice in tests; idempotency keeps the
		// concurrent test suite from panicking on the duplicate-name
		// check.
		name := shortTypeName(r)
		if LookupRule(name) != nil {
			continue
		}
		RegisterRule(name, r)
	}
}

func registerBatchARules() {
	for _, r := range BatchAExpressionRules() {
		name := shortTypeName(r)
		if LookupRule(name) != nil {
			continue
		}
		RegisterRule(name, r)
	}
}

func registerMatchingRules() {
	for _, r := range MatchingRules() {
		name := shortTypeName(r)
		if LookupRule(name) != nil {
			continue
		}
		RegisterRule(name, r)
	}
}

// shortTypeName strips Go's package + pointer prefix from %T output.
// "*cascades.FilterMergeRule" → "FilterMergeRule". This is the Go analog of
// Java's `rule.getClass().getSimpleName()`, the spelling Java's
// `RecordQueryPlannerConfiguration.isRuleEnabled` and
// `setDisabledTransformationRuleNames` use — so a rule name is spelled the
// same in Go and in Java, and a `DISABLED_PLANNER_RULES` value written for
// one engine works unchanged on the other. Takes `any` because both
// ExpressionRule and ImplementationRule are named this way.
func shortTypeName(r any) string {
	t := typeNameForRegistry(r)
	for i := len(t) - 1; i >= 0; i-- {
		if t[i] == '.' || t[i] == '*' {
			return t[i+1:]
		}
	}
	return t
}
