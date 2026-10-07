package cascades

import "fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"

// ObservedRuleCall is one rule call as the planner ran it: the Go counterpart of what
// the target's rule trace (its PlannerEventListeners listener, the
// conformance planRuleTrace step) records per ended rule call. Rule is the
// rule's Java simple name — a conditional chain reports the inner rule that
// ran, never the wrapper, as Java's ConditionalTransformExpression applies
// its inner rules one at a time. Progress is Java's executeRuleCall notion: a
// new member inserted or a new partial match / constraint.
type ObservedRuleCall struct {
	Phase      PlannerPhase
	Rule       string
	Expression expressions.RelationalExpression
	Progress   bool
}

// SetRuleCallObserver installs a test-only observer of every rule call this
// planner makes (WS-F W6 step 1), on the planning goroutine. Nil removes it.
func (p *Planner) SetRuleCallObserver(observe func(ObservedRuleCall)) { p.ruleObserver = observe }
