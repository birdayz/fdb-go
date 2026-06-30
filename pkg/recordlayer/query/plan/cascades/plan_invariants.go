package cascades

import (
	"fmt"

	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// ValidatePlanInvariants checks RFC-164 WS-2 structural invariants on a fully
// extracted physical plan tree. It is a cheap, always-on backstop (run in the
// no-FDB plan harness + a fuzz target) that makes a whole class of malformed
// plans un-shippable rather than hunting them one PR at a time.
//
// Invariant (no dropped/nil child): a non-leaf plan node must have at least one
// child. The IN-LIMIT bug produced a Fetch/InJoin whose inner was left nil by a
// faulty relink; GetChildren() *masks* a nil inner as zero children, so such a
// node masquerades as a leaf. Comparing against the (small, fixed) set of
// genuine leaf plan types — the scan-like and value-generating plans that
// legitimately have no children — turns that masquerade into a loud failure. A
// compound-leaf rendering like TypeFilter(Scan) is unaffected: the TypeFilter
// node genuinely has its Scan child.
//
// The walk is on the materialized plan tree (not the expression tree): the
// malformed node is an eagerly-embedded plan snapshot with no live expression
// member, so only a plan-tree walk reaches it.
func ValidatePlanInvariants(plan plans.RecordQueryPlan) error {
	return validatePlanNode(plan, map[plans.RecordQueryPlan]struct{}{})
}

func validatePlanNode(plan plans.RecordQueryPlan, seen map[plans.RecordQueryPlan]struct{}) error {
	if plan == nil {
		return fmt.Errorf("plan-invariant: encountered a nil plan node")
	}
	if _, ok := seen[plan]; ok {
		return nil
	}
	seen[plan] = struct{}{}

	children := plan.GetChildren()
	if len(children) == 0 && !isGenuineLeafPlan(plan) {
		// %T (not Explain) — Explain on a malformed plan can itself panic
		// (some impls dereference a nil inner), and the type pinpoints the node.
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

// isGenuineLeafPlan reports whether plan legitimately has zero children: a scan-
// like or value-generating leaf. Every other plan type is a unary/n-ary operator
// that REQUIRES children, so zero children there means a dropped/nil inner.
//
// This set must stay in sync with the plan types whose GetChildren()
// unconditionally returns empty (grep the plans package). Drift fails LOUD, not
// silent: a new genuine leaf misclassified as non-leaf trips the invariant on a
// valid plan (caught by the corpus); a new operator is correctly guarded. RFC-164
// WS-3's RecordQueryPlanVisitor would make this exhaustiveness compile-time.
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
