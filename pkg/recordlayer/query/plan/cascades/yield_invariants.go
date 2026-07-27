package cascades

import (
	"fmt"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
)

// PlannerInvariantViolationError marks a structural invariant violation caught
// at rule-yield time: a nil expression, a quantifier over an unmemoized child,
// or a nil-inner shell. Every such failure is an engine bug, never a statement
// about the user's query.
//
// It exists so a caller can identify the FAMILY without parsing the message.
// The underlying text names Go types (it formats the offending expression with
// %T), so the SQL layer withholds it from users; a family that survives as a
// type is what keeps those failures triageable from a log line.
type PlannerInvariantViolationError struct{ Cause error }

// Error returns the underlying invariant message unchanged, so in-package
// diagnostics and tests are unaffected by the tagging.
func (e *PlannerInvariantViolationError) Error() string { return e.Cause.Error() }

// Unwrap returns the underlying invariant error.
func (e *PlannerInvariantViolationError) Unwrap() error { return e.Cause }

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
func verifyChildrenMemoized(expr expressions.RelationalExpression, reach *ReachabilityCollector) (err error) {
	// Every failure below is one family — a rule handed the memo an expression
	// that violates a structural invariant. Tagging it HERE, at the single
	// chokepoint that mints them, is what lets a caller name the family without
	// string-matching the message: the SQL layer withholds these messages from
	// users (they format expressions with %T), so the type is the only thing
	// left to triage from.
	defer func() {
		if err != nil {
			err = &PlannerInvariantViolationError{Cause: err}
		}
	}()
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
	//
	// "Has children" is answered by the plan's QUANTIFIERS, not GetChildren:
	// GetChildren dereferences each quantifier to its plan (IDENTITY resolution),
	// which for a deferred-winner plan (a set operation) reaches a multi-member
	// leg group whose winner is not stamped until after this yield — so resolving
	// it here would trip the singleton guard. A collapsed plan carries its
	// children AS quantifiers, so counting them detects the shell without any
	// resolution. Fall back to GetChildren only for a plan that is not its own
	// cascades expression (no quantifiers to consult).
	var hasChildren bool
	if rel, ok := plan.(expressions.RelationalExpression); ok {
		hasChildren = len(rel.GetQuantifiers()) != 0
	} else {
		hasChildren = len(plan.GetChildren()) != 0
	}
	if !hasChildren && !isGenuineLeafPlan(plan) {
		return fmt.Errorf(
			"yield-invariant: %T yielded a nil-inner shell (plan %T has no children) — "+
				"rules must construct a plan with its child already attached, the way Java "+
				"evaluates memoizePlan as a constructor argument", expr, plan)
	}
	return nil
}
