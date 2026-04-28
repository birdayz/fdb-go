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
// Each call returns a fresh slice — callers may mutate freely. Each
// element is a fresh rule instance — see NewXxxRule constructors for
// the per-call allocation contract.
func DefaultExpressionRules() []ExpressionRule {
	return []ExpressionRule{
		NewFilterMergeRule(),
		NewFilterDropTruePredicatesRule(),
		NewPushFilterThroughDistinctRule(),
		NewPushFilterThroughTypeFilterRule(),
		NewPushFilterThroughSortRule(),
		NewPushFilterThroughUnionRule(),
		NewPushFilterThroughIntersectionRule(),
		NewDistinctMergeRule(),
		NewDistinctOverSortElimRule(),
		NewDistinctOverUnionDedupRule(),
		NewTypeFilterMergeRule(),
		NewTypeFilterRedundantOverScanRule(),
		NewUnionMergeRule(),
		NewIntersectionMergeRule(),
		NewNoOpFilterRule(),
		NewProjectionMergeRule(),
		NewProjectionElimRule(),
		NewSortMergeRule(),
		NewSortDedupKeysRule(),
		NewUnsortedSortElimRule(),
		NewUnionSingletonElimRule(),
		NewIntersectionSingletonElimRule(),
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
