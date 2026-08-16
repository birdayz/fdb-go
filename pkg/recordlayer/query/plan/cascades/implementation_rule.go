package cascades

import (
	"context"

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
	Bindings  *matching.PlannerBindings
	Reference *expressions.Reference
	Context   PlanContext
	// RunContext is nil only for standalone rule tests.
	RunContext           context.Context
	Constraints          *ConstraintMap
	Stats                properties.StatisticsProvider
	memo                 *Memo
	yielded              []expressions.RelationalExpression
	constraintOnly       bool
	constraintPushedRefs []*expressions.Reference
	pendingConstraints   []pendingRequestedOrderingConstraint
	indexYieldedInMemo   bool
	// stagedInserts are InsertReExploring effects held for the same commit
	// boundary as the yields. An ExpressionRule running under this driver
	// through expressionRuleAdapter hands its own staged inserts here, so the
	// atomicity boundary is the OUTER call's — the one whose preflight can
	// still fail after the rule body returned.
	stagedInserts []stagedReExploringInsert
	err           error
}

type pendingRequestedOrderingConstraint struct {
	ref       *expressions.Reference
	orderings []*properties.RequestedOrdering
}

// Fail records the first rule-body failure. Drivers check Err before applying
// any staged yield or constraint effect.
func (c *ImplementationRuleCall) Fail(err error) {
	if c == nil || err == nil || c.err != nil {
		return
	}
	c.err = err
}

// AdoptStagedInserts takes over another call's held InsertReExploring effects,
// so a rule running under an adapter commits at the OUTER driver's boundary
// rather than at the inner one, which cannot see the outer preflight.
func (c *ImplementationRuleCall) AdoptStagedInserts(staged []stagedReExploringInsert) {
	if c == nil || len(staged) == 0 {
		return
	}
	c.stagedInserts = append(c.stagedInserts, staged...)
}

// CommitStagedInserts applies the held InsertReExploring effects. Drivers call
// it only after Err is clear and the whole batch has preflighted, and BEFORE
// publishing the yields, so a parent lands over complete children.
func (c *ImplementationRuleCall) CommitStagedInserts() {
	if c == nil {
		return
	}
	for _, s := range c.stagedInserts {
		if c.memo != nil {
			c.memo.InsertReExploring(s.ref, s.expr)
			continue
		}
		s.ref.Insert(s.expr)
	}
	c.stagedInserts = nil
}

// Err returns the first error reported by the rule body.
func (c *ImplementationRuleCall) Err() error {
	if c == nil {
		return nil
	}
	return c.err
}

// CancellationErr reports whether the owning planning run was canceled.
func (c *ImplementationRuleCall) CancellationErr() error {
	if c == nil || c.RunContext == nil {
		return nil
	}
	return c.RunContext.Err()
}

// CostModel returns the comparator a rule should use for internal best-plan
// selection: stats-aware when the planner threaded statistics, else the
// default-stats comparator. Mirrors ExpressionRuleCall.CostModel.
func (c *ImplementationRuleCall) CostModel() func(a, b expressions.RelationalExpression) bool {
	ctx := c.Context
	if c.Stats == nil {
		// Preserve the historical nil-context comparison while retaining only
		// an injected diagnostic sink.
		ctx = costModelDiagnosticsOnlyContext(ctx)
	}
	return NewPlanningCostModelLessWithContext(c.Stats, ctx)
}

// Yield records a final expression to be inserted into the
// Reference's final members after the rule completes.
func (c *ImplementationRuleCall) Yield(expr expressions.RelationalExpression) {
	if c.constraintOnly || c.Err() != nil || c.CancellationErr() != nil {
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
	if c.Err() != nil || c.CancellationErr() != nil {
		return
	}
	c.pendingConstraints = append(c.pendingConstraints, pendingRequestedOrderingConstraint{
		ref:       childRef,
		orderings: append([]*properties.RequestedOrdering(nil), orderings...),
	})
}

func (c *ImplementationRuleCall) applyPendingConstraints() {
	for _, pending := range c.pendingConstraints {
		if Set(c.Constraints, pending.ref, RequestedOrderingConstraintKey, pending.orderings) {
			c.constraintPushedRefs = append(c.constraintPushedRefs, pending.ref)
		}
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
	//
	// Shares its whole body with MemoizeMemberPlansFromOther. Until this
	// change it did NOT: it copied the source's property map wholesale
	// (`SetPlanProperties(source.GetPlanProperties())`) while its twin
	// restricted the map to the retained members — so a reference restricted
	// to one plan still reported the entire source group to anything reading
	// ToPlanPartitions, which walks the property map rather than the member
	// list. That is the same defect the twin was written to avoid, live here
	// across nine call sites. One implementation now, so the two cannot drift
	// apart again.
	restricted := newRestrictedFinalReference(
		"MemoizeFinalExpressionsFromOther", source, exprs, expressions.StageCanonical)
	// The new reference is the same constrained child domain narrowed to one
	// plan partition. Preserve the requested-ordering requirement that caused
	// that partition to be selected; otherwise its OptimizeGroup pass sees an
	// unconstrained singleton/group and can prune the ordered member the parent
	// is about to rely on. Java's partition reference is ordering-homogeneous,
	// so this copy is implicit there; Go's separately-keyed ConstraintMap needs
	// it stated explicitly.
	if orderings, ok := Get(c.Constraints, source, RequestedOrderingConstraintKey); ok {
		Set(c.Constraints, restricted, RequestedOrderingConstraintKey,
			append([]*properties.RequestedOrdering(nil), orderings...))
	}
	return restricted
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
func FireImplementationRule(rule ImplementationRule, ref *expressions.Reference, constraints ...*ConstraintMap) ([]expressions.RelationalExpression, error) {
	return FireImplementationRuleWithContext(rule, ref, nil, nil, constraints...)
}

// FireImplementationRuleWithContext is like FireImplementationRule but
// threads a PlanContext through to the rule call. The planner uses this
// to provide PK / match-candidate info to rules that need it.
func FireImplementationRuleWithContext(rule ImplementationRule, ref *expressions.Reference, ctx PlanContext, memo *Memo, constraints ...*ConstraintMap) ([]expressions.RelationalExpression, error) {
	var cm *ConstraintMap
	if len(constraints) > 0 {
		cm = constraints[0]
	}
	if ctx == nil {
		ctx = EmptyPlanContext()
	}
	var allYielded []expressions.RelationalExpression
	for _, member := range ref.AllMembers() {
		yielded, err := fireImplRuleOnMember(rule, ref, member, ctx, cm, memo)
		if err != nil {
			return nil, err
		}
		allYielded = append(allYielded, yielded...)

		if sel, ok := member.(*expressions.SelectExpression); ok && sel.ChildrenAsSet() {
			qs := sel.GetQuantifiers()
			if len(qs) >= 2 && sel.GetJoinType() != expressions.JoinLeftOuter &&
				qs[0].Kind() == expressions.QuantifierForEach &&
				qs[1].Kind() == expressions.QuantifierForEach {
				swapped := sel.WithSwappedQuantifiers()
				yielded, err := fireImplRuleOnMember(rule, ref, swapped, ctx, cm, memo)
				if err != nil {
					return nil, err
				}
				allYielded = append(allYielded, yielded...)
			}
		}
	}
	return allYielded, nil
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
) ([]expressions.RelationalExpression, error) {
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
		if err := call.Err(); err != nil {
			return nil, err
		}
		var batch *preparedReferenceBatch
		if len(call.yielded) > 0 {
			intents := make([]referenceMemberIntent, len(call.yielded))
			for i, y := range call.yielded {
				intents[i] = referenceMemberIntent{set: expressions.ReferenceFinalMembers, expression: y}
			}
			prepared, err := prepareReferenceMemberBatch(ref, intents)
			if err != nil {
				return nil, err
			}
			batch = prepared
		}
		// After EVERY fallible step, and before the parent members land. A clear
		// Err only says the rule BODY succeeded; prepare can still reject the
		// batch, and an insert published above it survives that rejection.
		call.CommitStagedInserts()
		if batch != nil {
			if err := batch.commit(); err != nil {
				return nil, err
			}
		}
		call.applyPendingConstraints()
		for _, y := range call.yielded {
			if call.indexYieldedInMemo && memo != nil {
				memo.AddExpression(ref, y)
			}
		}
		yielded = append(yielded, call.yielded...)
	}
	return yielded, nil
}
