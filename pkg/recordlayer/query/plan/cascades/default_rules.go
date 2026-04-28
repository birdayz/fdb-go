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
func DefaultExpressionRules() []ExpressionRule {
	return []ExpressionRule{
		NewFilterMergeRule(),
		NewFilterDropTruePredicatesRule(),
		NewDistinctMergeRule(),
		NewTypeFilterMergeRule(),
		NewUnionMergeRule(),
		NewIntersectionMergeRule(),
		NewNoOpFilterRule(),
		NewProjectionElimRule(),
		NewUnsortedSortElimRule(),
		NewUnionSingletonElimRule(),
		NewIntersectionSingletonElimRule(),
	}
}
