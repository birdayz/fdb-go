package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
)

// ImplementationRule is a rule that runs during PhasePlanning.
// Like ExpressionRule, it yields expressions into Members via
// ref.Insert(). It operates on expression partitions and creates
// physical plan alternatives.
//
// Ports Java's ImplementationCascadesRule.
type ImplementationRule interface {
	Matcher() matching.BindingMatcher
	OnMatch(call *ImplementationRuleCall)
}

// ImplementationRuleCall provides the restricted API that
// ImplementationRules are allowed to use. It extends the base
// RuleCall with Memoizer and Yield operations.
//
// Ports Java's ImplementationCascadesRuleCall.
type ImplementationRuleCall struct {
	Bindings             *matching.PlannerBindings
	Reference            *expressions.Reference
	Context              PlanContext
	Constraints          *ConstraintMap
	Stats                properties.StatisticsProvider
	memo                 *Memo
	yielded              []expressions.RelationalExpression
	constraintOnly       bool
	constraintPushedRefs []*expressions.Reference
}

// CostModel returns the comparator a rule should use for internal best-plan
// selection: stats-aware when the planner threaded statistics, else the
// default-stats comparator. Mirrors ExpressionRuleCall.CostModel.
func (c *ImplementationRuleCall) CostModel() func(a, b expressions.RelationalExpression) bool {
	if c.Stats != nil {
		return NewPlanningCostModelLessWithContext(c.Stats, c.Context)
	}
	return PlanningCostModelLess
}

// Yield records a final expression to be inserted into the
// Reference's final members after the rule completes.
func (c *ImplementationRuleCall) Yield(expr expressions.RelationalExpression) {
	if c.constraintOnly {
		return
	}
	c.yielded = append(c.yielded, expr)
}

// YieldFinalExpression is an alias for Yield — matches Java's
// FinalYields.yieldFinalExpression naming.
func (c *ImplementationRuleCall) YieldFinalExpression(expr expressions.RelationalExpression) {
	c.Yield(expr)
}

// GetRequestedOrderings returns the requested orderings for this
// Reference, if set by a parent rule. Returns nil if no ordering
// constraint is set.
func (c *ImplementationRuleCall) GetRequestedOrderings() []*properties.RequestedOrdering {
	orderings, ok := Get(c.Constraints, c.Reference, RequestedOrderingConstraintKey)
	if !ok {
		return nil
	}
	return orderings
}

// IsConstraintOnly returns true when the rule is firing during the
// top-down constraint-propagation pass (PLANNING Phase 1). Rules that
// only push constraints should check this and skip implementation work.
func (c *ImplementationRuleCall) IsConstraintOnly() bool {
	return c.constraintOnly
}

// PushConstraint pushes requested orderings to a child Reference,
// COMBINING with anything already pushed there (Java
// ConstraintsMap.pushProperty → PlannerConstraint.combine — a
// subsumption union). A blind replace let the LAST parent pushing onto a
// shared child clobber every other parent's requirement, and the
// requested-ordering winner retention in OptimizeGroupTask would then
// prune the clobbered parents' ordered alternatives.
func (c *ImplementationRuleCall) PushConstraint(
	childRef *expressions.Reference,
	orderings []*properties.RequestedOrdering,
) {
	// Set IS the combiner (Java pushProperty): the former pre-combine
	// here duplicated it. A SUBSUMED push schedules no re-exploration —
	// Java's empty Optional schedules nothing; enqueueing on every push
	// held groups in re-exploration for constraint no-ops.
	if Set(c.Constraints, childRef, RequestedOrderingConstraintKey, orderings) {
		c.constraintPushedRefs = append(c.constraintPushedRefs, childRef)
	}
}

// MemoizeFinalExpressionsFromOther creates a new Reference containing
// only the specified expressions (which must already be members of
// `source`). The new Reference holds them as final members —
// disentangled from the shared DAG.
//
// Ports Java's FinalMemoizer.memoizeFinalExpressionsFromOther.
func (c *ImplementationRuleCall) MemoizeFinalExpressionsFromOther(
	source *expressions.Reference,
	exprs []expressions.RelationalExpression,
) *expressions.Reference {
	// FINAL set, CANONICAL stage. These are plans, and Java's memoizePlan
	// lands plans in the final set (Reference.ofFinalExpressions) — minting
	// via InitialOf put them in the EXPLORATORY set, so every reference built
	// here had an empty FinalMembers() and the name said the opposite of what
	// happened. FinalOf is not the right constructor either: it also stamps
	// StagePlanned, which is the SPINE-PIN decision, not the memoize
	// decision, and forcing it here changes what ExploreGroupTask does with
	// the reference.
	var ref *expressions.Reference
	for i, e := range exprs {
		if i == 0 {
			ref = expressions.FinalOfAtStage(e, expressions.StageCanonical)
		} else {
			ref.InsertFinal(e)
		}
	}
	if ref == nil {
		ref = &expressions.Reference{}
	}
	if source != nil {
		ref.SetPlanProperties(source.GetPlanProperties())
	}
	return ref
}

// MemoizeFinalExpression creates a new Reference holding expr as its single
// FINAL member — Java's memoizePlan (Reference.ofFinalExpressions). Stage
// stays CANONICAL: see MemoizeFinalExpressionsFromOther for why the final-set
// placement and the planner stage are separate decisions.
func (c *ImplementationRuleCall) MemoizeFinalExpression(
	expr expressions.RelationalExpression,
) *expressions.Reference {
	return expressions.FinalOfAtStage(expr, expressions.StageCanonical)
}

// FireImplementationRule runs an ImplementationRule against a Reference,
// matching each member and collecting yielded expressions.
// Returns the yielded expressions (also inserted into ref.Members).
func FireImplementationRule(rule ImplementationRule, ref *expressions.Reference, constraints ...*ConstraintMap) []expressions.RelationalExpression {
	return FireImplementationRuleWithContext(rule, ref, nil, nil, constraints...)
}

// FireImplementationRuleWithContext is like FireImplementationRule but
// threads a PlanContext through to the rule call. The planner uses this
// to provide PK / match-candidate info to rules that need it.
func FireImplementationRuleWithContext(rule ImplementationRule, ref *expressions.Reference, ctx PlanContext, memo *Memo, constraints ...*ConstraintMap) []expressions.RelationalExpression {
	var cm *ConstraintMap
	if len(constraints) > 0 {
		cm = constraints[0]
	}
	if ctx == nil {
		ctx = EmptyPlanContext()
	}
	var allYielded []expressions.RelationalExpression
	for _, member := range ref.AllMembers() {
		allYielded = append(allYielded, fireImplRuleOnMember(rule, ref, member, ctx, cm, memo)...)

		if sel, ok := member.(*expressions.SelectExpression); ok && sel.ChildrenAsSet() {
			qs := sel.GetQuantifiers()
			if len(qs) >= 2 && sel.GetJoinType() != expressions.JoinLeftOuter &&
				qs[0].Kind() == expressions.QuantifierForEach &&
				qs[1].Kind() == expressions.QuantifierForEach {
				swapped := sel.WithSwappedQuantifiers()
				allYielded = append(allYielded, fireImplRuleOnMember(rule, ref, swapped, ctx, cm, memo)...)
			}
		}
	}
	return allYielded
}

// fireImplRuleOnMember runs a single implementation rule against a single
// member, inserting yielded expressions into ref.Members and returning
// them. Extracted to avoid duplication between normal and
// ChildrenAsSet-permuted firing.
func fireImplRuleOnMember(
	rule ImplementationRule,
	ref *expressions.Reference,
	member expressions.RelationalExpression,
	ctx PlanContext,
	cm *ConstraintMap,
	memo *Memo,
) []expressions.RelationalExpression {
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), member)
	var yielded []expressions.RelationalExpression
	for _, b := range bindings {
		call := &ImplementationRuleCall{
			Bindings:    b,
			Reference:   ref,
			Context:     ctx,
			Constraints: cm,
			memo:        memo,
		}
		rule.OnMatch(call)
		for _, y := range call.yielded {
			ref.InsertFinal(y)
		}
		yielded = append(yielded, call.yielded...)
	}
	return yielded
}
