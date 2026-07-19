package cascades

import (
	"fmt"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
)

// verifyChildrenMemoized ports Java's CascadesRuleCall.verifyChildrenMemoized
// (CascadesRuleCall.java:221-241) — an UNCONDITIONAL check (a `Verify`, not a
// debug-only sanity check) that a rule never hands the memo an expression
// whose children are not already memoized.
//
// Java gets most of this property structurally and only asserts the rest: a
// child there is a dereference through a `@Nonnull final Reference`
// (Quantifier.java:467) with no setter anywhere in plan/, so a parent cannot
// exist without its child. Go stores the parent→child edge TWICE — once as a
// quantifier's Reference, once as a pointer inside the embedded
// RecordQueryPlan — and nothing in the type system forces the two to agree.
// A "shell" is exactly the state where they disagree: a quantifier pointing at
// a real child, over a plan whose inner is nil, to be relinked later at
// extraction.
//
// That deferred relink is the root cause of a recurring defect family (RFC-183
// §1). This check closes the window at its source: a rule that would yield a
// shell fails LOUDLY here instead of producing a plan that extraction has to
// repair — or, when the repair misfires, a plan that silently returns wrong
// rows.
//
// Always-on, matching Java. It is O(children) on an expression the planner was
// about to insert anyway, so it costs nothing measurable, and
// ValidatePlanInvariants (plan_invariants.go) is the established precedent for
// an always-on structural backstop in this package. The two are complementary:
// this one guards the EXPRESSION at yield time (the source), that one guards
// the extracted PLAN tree (the sink).
// reach is the optional per-Planner reachability tally; nil collects nothing.
// It is a PARAMETER rather than package state because a tally shared across
// concurrent planners is not a measurement — see ReachabilityCollector.
func verifyChildrenMemoized(expr expressions.RelationalExpression, reach *ReachabilityCollector) error {
	if expr == nil {
		return fmt.Errorf("yield-invariant: rule yielded a nil expression")
	}
	for i, q := range expr.GetQuantifiers() {
		ref := q.GetRangesOver()
		if ref == nil {
			return fmt.Errorf(
				"yield-invariant: %T quantifier %d ranges over no reference — its child was never memoized", expr, i)
		}
		if len(ref.AllMembers()) == 0 {
			return fmt.Errorf(
				"yield-invariant: %T quantifier %d ranges over an empty reference — its child was never memoized", expr, i)
		}
	}
	// Reachability is ACCOUNTED here, not enforced: RFC-183 inherited a
	// population of unreachable edges and is driving it to zero. See
	// plan_reachability.go for why it graduates to a hard error only at zero.
	reach.record(expr)

	return verifyNoShell(expr)
}

// verifyNoShell reports the nil-inner-shell state: a physical expression whose
// embedded plan snapshot is structurally incomplete while its quantifiers say
// otherwise.
//
// Split out from verifyChildrenMemoized so the shell property can be asserted
// on its own in tests without also requiring a populated memo — the two
// failures have different causes and should not be conflated in a message.
func verifyNoShell(expr expressions.RelationalExpression) error {
	pe, ok := expr.(physicalPlanExpression)
	if !ok {
		return nil // logical expression — no embedded plan to be a shell
	}
	plan := pe.GetRecordQueryPlan()
	if plan == nil {
		return fmt.Errorf("yield-invariant: physical expression %T carries no plan", expr)
	}
	// Structurally incomplete = a non-leaf carrying no children, per the
	// plan-invariant authority (isGenuineLeafPlan).
	if len(plan.GetChildren()) == 0 && !isGenuineLeafPlan(plan) {
		return fmt.Errorf(
			"yield-invariant: %T yielded a nil-inner shell (plan %T has no children) — "+
				"rules must construct a plan with its child already attached, the way Java "+
				"evaluates memoizePlan as a constructor argument", expr, plan)
	}
	return nil
}
