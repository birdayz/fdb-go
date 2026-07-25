package cascades

import (
	"fmt"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
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
	if len(children) == 0 && !isGenuineLeafPlan(plan) {
		// %T (not Explain) — Explain on a malformed plan can itself panic (some
		// impls dereference a nil inner), and the type pinpoints the node.
		return fmt.Errorf("plan-invariant: non-leaf plan %T has no children — a relink dropped its inner (a nil child masked by GetChildren)", plan)
	}
	if inJoin, ok := plan.(*plans.RecordQueryInJoinPlan); ok {
		if err := validateInJoinSortedClaim(inJoin); err != nil {
			return err
		}
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

// validateInJoinSortedClaim checks that a RecordQueryInJoinPlan claiming
// IsSorted() actually carries values in that order — the invariant that
// keeps sortInJoinValues (in_source.go) from rotting back into a landmine:
// a future construction site that sets sorted=true without sorting the
// values it hands to SetInValues would trip this the moment the plan is
// extracted, in the SAME always-on paths that already gate every other
// structural invariant (the no-FDB plan harness, the production generator,
// and FuzzPlanner_Invariants).
//
// Only checked when the plan carries 2+ concrete values: an unsorted claim
// over 0 or 1 values, or over a runtime source with no plan-time value list
// (GetInValues() empty), is vacuously satisfiable and not this invariant's
// concern — see in_source.go's sortInJoinValues doc for why the
// InSourceParameter/InSourceComparand case has no plan-time list to check
// today.
func validateInJoinSortedClaim(p *plans.RecordQueryInJoinPlan) error {
	if !p.IsSorted() {
		return nil
	}
	vals := p.GetInValues()
	if len(vals) < 2 {
		return nil
	}
	reverse := p.IsReverse()
	for i := 1; i < len(vals); i++ {
		c, err := values.CompareOrdered(vals[i-1], vals[i])
		if err != nil {
			return fmt.Errorf("plan-invariant: InJoin claims sorted but its values are not comparable at index %d: %w (values=%#v)", i, err, vals)
		}
		if reverse {
			c = -c
		}
		if c > 0 {
			return fmt.Errorf("plan-invariant: InJoin claims sorted(reverse=%v) but values are not in that order at index %d (values=%#v)", reverse, i, vals)
		}
	}
	return nil
}

// NOTE: n-ary set ops (Union/Intersection/MergeSortUnion/MultiIntersection/…)
// are deliberately NOT exempted. A dropped leg most often shows as a nil ELEMENT
// (caught by the per-child check), but a relink that empties the whole leg slice
// yields zero children — and that is a TRUE positive, the n-ary analog of the
// IN-LIMIT unary inner-drop. The planner never emits a legitimate 0-leg set op
// (a 1-leg op is simplified to its leg), so flagging it cannot false-positive.
