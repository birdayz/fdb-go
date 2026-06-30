package cascades

import (
	"fmt"

	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// ValidatePlanInvariants checks RFC-164 WS-2 structural invariants on a fully
// extracted physical plan tree. It is a cheap, always-on backstop — run in the
// no-FDB plan harness, the production generator, and a fuzz target — that makes a
// whole class of malformed plans un-shippable rather than hunting them one PR at
// a time.
//
// It has TWO detectors for a relink that dropped a child:
//
//   - Empty-children: a non-leaf plan with zero children. A unary operator whose
//     inner was left nil (the IN-LIMIT bug) has its nil masked by GetChildren
//     (which returns empty for a nil inner), so it masquerades as a leaf. Comparing
//     against the fixed set of genuinely-childless plan types unmasks it.
//   - Nil-in-slice: a fixed-arity operator (NLJ, FlatMap, recursive joins/unions)
//     returns a length-N slice whose dropped child is a nil ELEMENT rather than a
//     shorter slice; the per-child nil check below catches that.
//
// It walks the PLAN tree, not the expression tree: the malformation lives in the
// eagerly-materialized plan SNAPSHOT, while the live expression member (the
// wrapper's quantifier) still points at a healthy reference — so an
// expression-tree walk traverses the healthy edge and never reaches the break.
func ValidatePlanInvariants(plan plans.RecordQueryPlan) error {
	return validatePlanNode(plan, map[plans.RecordQueryPlan]struct{}{})
}

func validatePlanNode(plan plans.RecordQueryPlan, seen map[plans.RecordQueryPlan]struct{}) error {
	if plan == nil {
		return fmt.Errorf("plan-invariant: encountered a nil plan node")
	}
	if _, ok := seen[plan]; ok {
		return nil // shared sub-plan (DAG) or cycle guard
	}
	seen[plan] = struct{}{}

	children := plan.GetChildren()
	if len(children) == 0 && !childlessAllowed(plan) {
		// %T (not Explain) — Explain on a malformed plan can itself panic (some
		// impls dereference a nil inner), and the type pinpoints the node.
		return fmt.Errorf("plan-invariant: non-leaf plan %T has no children — a relink dropped its inner (a nil child masked by GetChildren)", plan)
	}
	for _, c := range children {
		if c == nil {
			return fmt.Errorf("plan-invariant: %T has a nil child", plan)
		}
		if err := validatePlanNode(c, seen); err != nil {
			return err
		}
	}
	return nil
}

// childlessAllowed reports whether plan may legitimately have zero children.
func childlessAllowed(plan plans.RecordQueryPlan) bool {
	return isGenuineLeafPlan(plan) || isNArySetOpPlan(plan)
}

// isGenuineLeafPlan reports whether plan is a scan-/value-producing leaf that
// always has zero children. Every other unary operator REQUIRES a child, so zero
// children there means a dropped/nil inner.
//
// This set must stay in sync with the plan types whose GetChildren()
// unconditionally returns empty (grep the plans package). Two drift directions
// fail LOUD: a new leaf misclassified as non-leaf trips the invariant on a valid
// plan (caught by the corpus), and a new operator is correctly guarded. One
// direction is silent and contrived: if an EXISTING leaf type later gains a real
// child but stays listed here, a dropped child on it is exempted — RFC-164 WS-3's
// RecordQueryPlanVisitor (type-encoded leaf-ness) is the durable fix.
func isGenuineLeafPlan(plan plans.RecordQueryPlan) bool {
	switch plan.(type) {
	case *plans.RecordQueryScanPlan,
		*plans.RecordQueryIndexPlan,
		*plans.RecordQueryAggregateIndexPlan,
		*plans.RecordQueryVectorIndexPlan,
		*plans.RecordQueryTextIndexPlan,
		*plans.RecordQueryTempTableScanPlan,
		*plans.RecordQueryLoadByKeysPlan,
		*plans.RecordQueryTableFunctionPlan,
		*plans.RecordQueryExplodePlan,
		*plans.RecordQueryValuesPlan:
		return true
	}
	return false
}

// isNArySetOpPlan reports whether plan is an n-ary set operation. These hold
// their legs in a slice, so a leg drop shows as a nil ELEMENT (caught by the
// per-child check), not as zero children. A genuinely EMPTY (zero-leg) set op is
// exempted from the empty-children check: the planner never emits one (a 1-leg
// set op is simplified to its leg, never to a 0-leg shell — Graefe), and the
// executor returns an empty cursor for zero inputs anyway (codex), so flagging it
// would risk a production false positive for no real coverage.
func isNArySetOpPlan(plan plans.RecordQueryPlan) bool {
	switch plan.(type) {
	case *plans.RecordQueryUnionPlan,
		*plans.RecordQueryUnorderedUnionPlan,
		*plans.RecordQueryIntersectionPlan,
		*plans.RecordQueryMergeSortUnionPlan,
		*plans.RecordQueryMultiIntersectionOnValuesPlan,
		*plans.RecordQuerySelectorPlan,
		*plans.RecordQueryComparatorPlan:
		return true
	}
	return false
}
