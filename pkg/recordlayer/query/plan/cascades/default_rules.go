package cascades

// DefaultExpressionRules returns the curated rule set the seed
// optimiser fires by default. Order matters within a fixpoint pass:
// merge-rules run first (collapse nested operators of the same kind),
// then no-op-elimination rules run on the merged shapes. Order across
// passes doesn't matter because FixpointApply runs each rule against
// every member each iteration.
//
// As Track B5 Batch A rules port (PrimaryScanRule, ImplementFilterRule,
// etc.), they join this list. The rules here today are uncontroversial
// logical-to-logical rewrites that don't need cost-aware decisions.
//
// **Alias rebasing gap (B5 follow-on)**: every Push*Through* and
// Pull*Above* rule in this set moves a Filter (or TypeFilter) across
// a Quantifier boundary without rewriting the moved operator's
// internal alias references. Predicates and Values inside the moved
// operator continue to reference the OLD outer Quantifier's alias
// after the structural transformation. Harmless in the seed (no
// evaluator descends into the rewritten sub-tree to use those
// aliases), but rules that inspect correlation structure
// (GetCorrelatedToWithoutChildren) on a pushed predicate would
// see stale aliases. The proper fix — TranslationMap-based rebasing
// — lands with B5 Batch A's physical-implementation rules. Until
// then, push/pull rules are seed-correct for row-set semantics but
// shouldn't be combined with correlation-aware rules.
//
// **ProjectionMergeRule's seed-only soundness**: the merge
// `Projection(P1) over Projection(P2) over X → Projection(P1) over X`
// is sound only because the seed's LogicalProjectionExpression's
// GetResultValue() passes the inner row through (projection is a
// pure side channel). When Track C materialises projections — i.e.
// projection actually narrows the row shape — P1's Values may
// reference computed columns that only exist in P2's output. At
// that point the rule needs a column-substitution rewrite path or
// it must be removed from the default set. See ProjectionMergeRule's
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
		NewPushFilterThroughSortRule(),
		NewPushFilterThroughUnionRule(),
		NewPushFilterThroughIntersectionRule(),
		NewPushFilterThroughGroupByRule(),
		NewPushFilterThroughProjectionRule(),
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
		NewPullCommonFilterAboveUnionRule(),
		NewIntersectionMergeRule(),
		NewPullCommonFilterAboveIntersectionRule(),
		NewNoOpFilterRule(),
		NewProjectionMergeRule(),
		NewProjectionElimRule(),
		NewPushProjectionBelowJoinRule(),
		NewPullFilterAboveProjectionRule(),
		NewSortMergeRule(),
		NewSortDedupKeysRule(),
		NewSortConstantKeysElimRule(),
		NewPullFilterAboveSortRule(),
		NewUnsortedSortElimRule(),
		NewPushOrderingThroughGroupByRule(),
		// PushOrderingThroughProjectionRule REMOVED: moved to PLANNING
		// phase as PushRequestedOrderingThroughProjectionRule (DefaultImplementationRules).
		// PushOrderingThroughFilterRule REMOVED (D-3): moved to PLANNING
		// phase as PushRequestedOrderingThroughFilterRule (DefaultImplementationRules).
		// PushOrderingThroughDistinctRule REMOVED (D-2): moved to PLANNING
		// phase as PushRequestedOrderingThroughDistinctRule (DefaultImplementationRules).
		// PushOrderingThroughUniqueRule REMOVED (D-2): moved to PLANNING
		// phase as PushRequestedOrderingThroughUniqueRule (DefaultImplementationRules).
		NewPushOrderingThroughUnionRule(),
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
		NewIndexIntersectionRule(),
		NewLimitMergeRule(),
		NewPushLimitThroughProjectionRule(),
		NewPushLimitThroughUnionRule(),
		NewNoOpLimitElimRule(),
		NewZeroLimitRule(),
		NewRemoveRangeOneRule(),
	}
}

// BatchAExpressionRules returns the B5 Batch A physical-implementation
// rules: rules that lower a logical RelationalExpression to a physical
// RecordQueryPlan via the per-shape physical wrapper bridges.
//
// These rules are NOT part of DefaultExpressionRules — keeping the two
// sets separate mirrors Java's logical/physical rule split. The
// planner driver decides whether to fire physical-implementation rules
// (when an executable plan is the goal) or only logical rewrites
// (when the goal is plan-rewrite analysis).
//
// Compose with: append(DefaultExpressionRules(), BatchAExpressionRules()...)
//
// Currently 6 read-side implement rules ported (PrimaryScanRule,
// ImplementFilterRule, ImplementTypeFilterRule, ImplementUnionRule,
// ImplementIntersectionRule). ImplementSortRule and ImplementDistinctRule
// moved to DefaultImplementationRules (PLANNING phase) per Java. Remaining: covering-index +
// MergeFetchIntoCoveringIndexRule + index-equality / range rules —
// all gated on MatchCandidate / IndexAccessHint infrastructure
// (per RFC-022).
func BatchAExpressionRules() []ExpressionRule {
	return []ExpressionRule{
		NewPrimaryScanRule(),
		NewImplementValuesRule(),
		NewImplementProjectionRule(),
		NewImplementFilterRule(),
		NewImplementIndexScanRule(),
		NewOrderedIndexScanRule(),
		NewOrderedPrimaryScanRule(),
		// ImplementDistinctRule REMOVED (D-3): Java's ImplementDistinctRule
		// is PLANNING-phase only. Distinct implementation + elimination now
		// happens exclusively in ImplementDistinctFinalRule (DefaultImplementationRules).
		NewImplementTypeFilterRule(),
		NewImplementUnionRule(),
		NewImplementIntersectionRule(),
		// SortOverOrderedElimRule REMOVED (D-1): Java's RemoveSortRule
		// is PLANNING-phase only. Sort elimination now happens exclusively
		// in ImplementSortRule (DefaultImplementationRules).
		NewImplementStreamingAggregationRule(),
		NewImplementHashAggregationRule(),
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
// PLANNING phase. FinalizeExpressionsRule is the catch-all; the
// specific rules fire before it for expressions they recognize.
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
		NewFinalizeExpressionsRule(),
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

// MatchingRules returns the matching rules that seed the partial-match
// infrastructure. These fire during the EXPLORE phase alongside
// expression rules but serve a different purpose: they establish
// PartialMatch instances between the query graph and MatchCandidate
// traversals, which are then consumed by implementation rules to
// produce index-scan plans.
//
// Mirrors Java's PlanningRuleSet.MATCHING_RULES:
//   - MatchLeafRule: seeds leaf-to-leaf matches
//   - MatchIntermediateRule: propagates matches upward (future)
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
// "*cascades.FilterMergeRule" → "FilterMergeRule".
func shortTypeName(r ExpressionRule) string {
	t := typeNameForRegistry(r)
	for i := len(t) - 1; i >= 0; i-- {
		if t[i] == '.' || t[i] == '*' {
			return t[i+1:]
		}
	}
	return t
}
