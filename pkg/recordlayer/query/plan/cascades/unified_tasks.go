package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
)

// InitiatePlannerPhaseTask starts a planner phase. Pushed once per phase.
// LIFO ordering ensures: ExploreGroup fires first, then OptimizeGroup,
// then the next phase's InitiatePlannerPhaseTask.
// Mirrors Java's CascadesPlanner.InitiatePlannerPhase.
type InitiatePlannerPhaseTask struct {
	Phase   PlannerPhase
	RootRef *expressions.Reference
}

func (t *InitiatePlannerPhaseTask) Run(p *Planner) {
	p.activePhase = t.Phase
	if t.Phase.HasNextPhase() {
		p.push(&InitiatePlannerPhaseTask{Phase: t.Phase.NextPhase(), RootRef: t.RootRef})
	}

	// Before PLANNING starts, adjust partial matches from REWRITING.
	if t.Phase == PhasePlanning {
		AdjustMatches(t.RootRef)
		if p.memo != nil {
			p.memo.MarkPlanningActive()
		}
	}

	p.push(&OptimizeGroupTask{Phase: t.Phase, Ref: t.RootRef})
	p.push(&ExploreGroupTask{Phase: t.Phase, Ref: t.RootRef})
}

// ExploreGroupTask explores a Reference within a phase. If the Reference's
// stage is behind the phase's target, it calls advancePlannerStage to
// transition. Then pushes exploration tasks for all members.
// Mirrors Java's CascadesPlanner.ExploreGroup.
type ExploreGroupTask struct {
	Phase PlannerPhase
	Ref   *expressions.Reference
}

func (t *ExploreGroupTask) Run(p *Planner) {
	if t.Ref == nil {
		return
	}

	targetStage := t.Phase.TargetStage()
	refStage := t.Ref.Stage()

	if targetStage != refStage {
		if targetStage.Precedes(refStage) {
			return
		}
		if len(t.Ref.FinalMembers()) > 0 {
			// WS-P stage (c) status: the REWRITING OptimizeInputs
			// routing prunes parent-chain-optimized groups to their
			// winner before this boundary (Java's covered case). A
			// UNIVERSAL forced prune here was attempted and reverted:
			// canonical alternatives Go's PLANNING cannot re-derive
			// (Java re-derives via its PLANNING rule set) were lost —
			// the RFC-153 buried-leg and cross-join-EXISTS shapes lost
			// their implementable form. Full prune-to-1 requires
			// PLANNING re-derivation parity first; until then
			// unoptimized groups cross with their full canonical set.
			t.Ref.AdvancePlannerStage(targetStage)
		} else {
			t.Ref.SetStage(targetStage)
		}
	}

	// Round tripwire (WS-P stage (d)): under EPOCH convergence rounds
	// happen only on verdict-gated constraint growth, and both constraint
	// lattices are finite chains — convergence is structural, so the old
	// load-level cap (10) is obsolete. The bound stays only as a LOUD
	// divergence tripwire (a combine that always reports growth, a
	// tick leak) far above any real workload; never a silent commit of a
	// half-explored group.
	const maxRoundsPerRef = 100
	if t.Ref.NeedsExploration() && t.Ref.ExplRounds() >= maxRoundsPerRef {
		p.capErr = ErrPlannerRoundCapHit
		return
	}
	if !t.Ref.NeedsExploration() {
		t.Ref.CommitExploration()
		if t.Phase == PhasePlanning {
			computeRefPlanProperties(t.Ref)
		}
		return
	}

	// Observed-rounds evidence: exported so stress/conformance runs can
	// show how far below the divergence tripwire real workloads sit.
	if r := t.Ref.ExplRounds() + 1; r > p.maxObservedExplRounds {
		p.maxObservedExplRounds = r
	}

	p.push(&ExploreGroupTask{Phase: t.Phase, Ref: t.Ref})

	// FINALS ROUTING (Java ExploreGroup: getFinalExpressions() →
	// exploreExpressionAndOptimizeInputs). Physical yields insert as
	// finals ONLY, so this loop is what explores them; the
	// isExploratoryMember skip covers finals that are ALSO canonical
	// members (FinalizeExpressionsRule promotes the same pointer),
	// which the member loop below owns.
	for _, expr := range t.Ref.FinalMembers() {
		if isExploratoryMember(t.Ref, expr) {
			continue // also an exploratory member — the member loop owns it
		}
		// OptimizeInputs routing per phase (Java ExploreGroup routes
		// EVERY final through exploreExpressionAndOptimizeInputs):
		//   - REWRITING (WS-P stage (c)): finals are the canonical
		//     LOGICAL forms FinalizeExpressionsRule promoted;
		//     OptimizeInputs → OptimizeGroup prunes each group to its
		//     REWRITING winner so the stage boundary crosses
		//     pruned (Java Verify's property).
		//   - PLANNING: physical finals only — the correlated-leg
		//     muzzle (a logical parent must not drive standalone child
		//     pruning with the correlation unbound; see the member-loop
		//     comment below).
		if t.Phase == PhaseRewriting || (t.Phase == PhasePlanning && isPhysical(expr)) {
			p.push(&OptimizeInputsTask{Phase: t.Phase, Ref: t.Ref, Expr: expr})
		}
		// PLANNING explores PHYSICAL finals only (match re-consumption).
		// A LOGICAL PLANNING final is an UNSAFE compensation
		// (consumeMatchPartitions' InsertFinal arm) — a fail-to-plan
		// sentinel that rules must never physicalize: exploring it let
		// ImplementFilterRule build top-K-before-filter (silent wrong
		// rows; the vector unsafe-residual pin is the red shape).
		if t.Phase == PhasePlanning && !isPhysical(expr) {
			continue
		}
		p.push(&ExploreExprTask{Phase: t.Phase, Ref: t.Ref, Expr: expr})
	}

	// Explore ALL members each round (Java ExploreGroup): rounds are
	// EPOCH-bounded now — a round runs only on first visit or after a
	// verdict-gated constraint push — and re-fired rules' yields hit the
	// memo dedup, so re-exploration is idempotent. The former
	// only-new-members slice was the member-count model's optimization.
	for _, expr := range t.Ref.Members() {
		// OptimizeInputs only for PHYSICAL (plan) members — the 1:1 port of Java's
		// CascadesPlanner. Java constructs OptimizeInputs in exactly one place
		// (CascadesPlanner.java:524), and its only callers push it ONLY for final/plan
		// expressions: ExploreGroup splits getFinalExpressions()→…AndOptimizeInputs vs
		// getExploratoryExpressions()→exploreExpression (:744-748), and executeRuleCall
		// makes the same new-final vs new-exploratory split (:1064-1070). Since
		// OptimizeInputsTask.Run pushes OptimizeGroupTask per child, gating it to
		// physical members means a child reference is pruned to a winner ONLY as the
		// inner of an IMPLEMENTED parent — so a CORRELATED leg is optimized only as the
		// inner child of the binding physical FlatMap, with the outer alias live, never
		// as a free-standing group with the correlation unbound. That structural
		// property (not a `refIsJoinLeg` flag) is why a correlated SUBSEL scan is never
		// stamped as a standalone winner → no 0-row. Child EXPLORATION is unaffected —
		// it is driven independently by ExploreExprTask step 4 (children's ExploreGroup),
		// not by OptimizeInputsTask — so this removes only premature standalone pruning.
		//
		// REWRITING (WS-P stage (c)): a member that is ALSO a final —
		// FinalizeExpressionsRule promotes the SAME expression object,
		// so canonical forms live in both sets — routes through
		// OptimizeInputs exactly like Java's getFinalExpressions split:
		// OptimizeGroup then prunes each group to its REWRITING winner
		// and the stage boundary crosses pruned (Java Verify's shape).
		if (t.Phase == PhaseRewriting && isFinalMember(t.Ref, expr)) ||
			(t.Phase == PhasePlanning && isPhysical(expr)) {
			p.push(&OptimizeInputsTask{Phase: t.Phase, Ref: t.Ref, Expr: expr})
		}
		p.push(&ExploreExprTask{Phase: t.Phase, Ref: t.Ref, Expr: expr})
	}

	t.Ref.StartExploration()
}

// ExploreExprTask pushes rule-transform tasks and child-exploration tasks
// for a single (group, expression) pair. Mirrors Java's AbstractExploreExpression.
type ExploreExprTask struct {
	Phase PlannerPhase
	Ref   *expressions.Reference
	Expr  expressions.RelationalExpression
}

func (t *ExploreExprTask) Run(p *Planner) {
	if t.Ref == nil || t.Expr == nil {
		return
	}

	// Root-operator rule selection (Java AbstractRuleSet.getRules): only
	// rules whose matcher can possibly match t.Expr's concrete type get
	// transform tasks — the rest would pop, type-assert, and fail anyway.
	exprIdx, implIdx := p.ruleIndexesForPhase(t.Phase)
	exprRules := exprIdx.rulesFor(t.Expr)
	implRules := implIdx.rulesFor(t.Expr)

	// 1. Push match-partition rules (fire LAST — deepest on LIFO).
	// Data access generation from PartialMatches.
	if t.Phase == PhasePlanning {
		p.pushDataAccessTasks(t.Ref, t.Expr)
	}

	// 2. Push non-preorder implementation rules.
	// Skip FinalizeExpressionsRule for expressions already in finals.
	for i := len(implRules) - 1; i >= 0; i-- {
		rule := implRules[i]
		if isPreOrderRule(rule) {
			continue
		}
		if _, ok := rule.(*FinalizeExpressionsRule); ok {
			if isFinalMember(t.Ref, t.Expr) {
				continue
			}
		}
		p.push(&TransformImplTask{Phase: t.Phase, Ref: t.Ref, Expr: t.Expr, Rule: rule})
	}

	// 3. Push non-preorder expression rules.
	for i := len(exprRules) - 1; i >= 0; i-- {
		p.push(&TransformExprTask{Phase: t.Phase, Ref: t.Ref, Expr: t.Expr, Rule: exprRules[i]})
	}

	// 4. Push ExploreGroup for each child quantifier's Reference.
	for _, q := range t.Expr.GetQuantifiers() {
		if childRef := q.GetRangesOver(); childRef != nil {
			p.push(&ExploreGroupTask{Phase: t.Phase, Ref: childRef})
		}
	}

	// 5. Push preorder implementation rules (fire FIRST — topmost on LIFO).
	for i := len(implRules) - 1; i >= 0; i-- {
		rule := implRules[i]
		if !isPreOrderRule(rule) {
			continue
		}
		p.push(&TransformImplTask{Phase: t.Phase, Ref: t.Ref, Expr: t.Expr, Rule: rule})
	}
}

// TransformExprTask fires a single ExpressionRule on a (group, expression)
// pair. Yields go to exploratory members (ref.Insert).
// Mirrors Java's TransformExpression for ExplorationCascadesRule.
type TransformExprTask struct {
	Phase PlannerPhase
	Ref   *expressions.Reference
	Expr  expressions.RelationalExpression
	Rule  ExpressionRule
}

func (t *TransformExprTask) Run(p *Planner) {
	if t.Ref == nil || t.Expr == nil || t.Rule == nil {
		return
	}
	if !t.Ref.ContainsExactly(t.Expr) {
		return
	}

	// During PLANNING, expression rules (BatchA) produce physical
	// wrappers. They land in the FINAL set only (Java's shape — the
	// dual insertion into the exploratory set was the member-count
	// convergence crutch; rule matching on finals runs through the
	// per-expression exploration tasks, and ContainsExactly admits
	// finals).
	var yieldFn func(expressions.RelationalExpression) bool
	if t.Phase == PhasePlanning {
		yieldFn = func(expr expressions.RelationalExpression) bool {
			// Same unconditional verifyChildrenMemoized as the two
			// ImplementationRuleCall yield sites. This one carries most of
			// the surface: as the comment above says, expression rules
			// produce PHYSICAL wrappers during PLANNING, and they reach the
			// memo here rather than through ImplementationRuleCall. Guarding
			// only the implementation path would leave the large majority of
			// physical yields unchecked and make the invariant decorative.
			if err := verifyChildrenMemoized(expr, p.reach); err != nil {
				p.capErr = err
				return false
			}
			return t.Ref.InsertFinal(expr)
		}
	}

	// Java's per-rule-call match cap counts ONE stream per rule invocation
	// (CascadesPlanner.execute: a single numMatches over bindMatches, which
	// enumerates quantifier permutations inside the same stream). The swapped
	// bind below is part of THIS rule call, so it shares the counter.
	numMatches := 0
	fireExprRule := func(expr expressions.RelationalExpression) {
		bindings := t.Rule.Matcher().BindMatches(matching.NewBindings(), expr)
		numMatches += len(bindings)
		if p.MaxNumMatchesPerRuleCall > 0 && numMatches > p.MaxNumMatchesPerRuleCall {
			p.capErr = ErrPlannerRuleMatchCapHit
			return
		}
		for _, b := range bindings {
			call := &ExpressionRuleCall{
				Bindings:    b,
				Reference:   t.Ref,
				Context:     p.ctx,
				Constraints: p.constraintMap,
				Stats:       p.stats,
				memo:        p.memo,
				yieldFn:     yieldFn,
			}
			// React to NEW PARTIAL MATCHES, not just new expressions. A matching
			// rule (MatchIntermediateRule / MatchLeafRule) seeds PartialMatches on
			// t.Ref without yielding any expression. Java's planner schedules a
			// follow-up task per new partial match (CascadesPlanner.executeRuleCall
			// iterating ruleCall.getNewPartialMatches()); Go's pushDataAccessTasks
			// instead runs inline at ExploreExprTask start — BEFORE the matching
			// rules have seeded this round's matches. So a match seeded here is only
			// consumed by a LATER, incidental re-exploration of t.Ref (e.g. when
			// ImplementFilterRule yields a physical filter member). When that
			// incidental trigger is absent — notably for an index-only filter, which
			// the Java !isIndexOnly() ImplementFilterRule gate legitimately
			// suppresses — the fully-bound match (e.g. a vector DistanceRank scan)
			// would never be consumed and the ref would stay logical. Re-run
			// data-access whenever this rule grew t.Ref's partial-match set, mirroring
			// Java's getNewPartialMatches() reaction. Self-bounded by the
			// match-growth re-entry guard inside pushDataAccessTasks (planner.go).
			var matchesBefore int
			if t.Phase == PhasePlanning {
				matchesBefore = len(t.Ref.GetAllPartialMatches())
			}
			t.Rule.OnMatch(call)
			if t.Phase == PhasePlanning && len(t.Ref.GetAllPartialMatches()) > matchesBefore {
				p.pushDataAccessTasks(t.Ref, t.Expr)
			}

			for _, newExpr := range call.Yielded() {
				// OptimizeInputs only for PHYSICAL yields — the other half of the B1
				// task-graph invariant (the executeRuleCall analog). Java's
				// CascadesPlanner.executeRuleCall (:1064-1070) splits ruleCall yields:
				// new FINAL expressions → OptimizeInputs, new EXPLORATORY → explore-only.
				// An ExpressionRule that yields a LOGICAL expression here (e.g.
				// PartitionBinarySelectRule's correlated SUBSEL SelectExpression) must NOT
				// drive child OptimizeGroupTask — otherwise a correlated leg could still be
				// pruned to a standalone winner from a logical parent, re-opening the
				// 0-row gap the muzzle covered. Gating this together with the
				// ExploreGroupTask site makes Go's OptimizeInputs scheduling match Java's
				// BOTH construction sites (ExploreGroup :744-748 + executeRuleCall :1064).
				if t.Phase == PhasePlanning && isPhysical(newExpr) {
					p.push(&OptimizeInputsTask{Phase: t.Phase, Ref: t.Ref, Expr: newExpr})
				}
				p.push(&ExploreExprTask{Phase: t.Phase, Ref: t.Ref, Expr: newExpr})
			}
		}
	}

	fireExprRule(t.Expr)

	if t.Phase == PhasePlanning && p.capErr == nil {
		if sel, ok := t.Expr.(*expressions.SelectExpression); ok && sel.ChildrenAsSet() {
			qs := sel.GetQuantifiers()
			if len(qs) >= 2 && sel.GetJoinType() != expressions.JoinLeftOuter &&
				qs[0].Kind() == expressions.QuantifierForEach &&
				qs[1].Kind() == expressions.QuantifierForEach {
				fireExprRule(sel.WithSwappedQuantifiers())
			}
		}
	}
}

// TransformImplTask fires a single ImplementationRule on a (group, expression)
// pair. Yields go to final members (ref.InsertFinal).
// Mirrors Java's TransformExpression for ImplementationCascadesRule.
type TransformImplTask struct {
	Phase PlannerPhase
	Ref   *expressions.Reference
	Expr  expressions.RelationalExpression
	Rule  ImplementationRule
}

func (t *TransformImplTask) Run(p *Planner) {
	if t.Ref == nil || t.Expr == nil || t.Rule == nil {
		return
	}
	if !t.Ref.ContainsExactly(t.Expr) {
		return
	}
	bindings := t.Rule.Matcher().BindMatches(matching.NewBindings(), t.Expr)
	// Java CascadesPlanner.isMaxNumMatchesPerRuleCallExceeded: one rule
	// invocation producing more matches than the bound is a complexity
	// blow-up; throw (here: capErr — tasks have no error channel). The
	// counter is ONE stream per rule call in Java (quantifier permutations
	// included), so the swapped bind below adds to it rather than resetting.
	numMatches := len(bindings)
	if p.MaxNumMatchesPerRuleCall > 0 && numMatches > p.MaxNumMatchesPerRuleCall {
		p.capErr = ErrPlannerRuleMatchCapHit
		return
	}
	for _, b := range bindings {
		call := &ImplementationRuleCall{
			Bindings:    b,
			Reference:   t.Ref,
			Context:     p.ctx,
			Constraints: p.constraintMap,
			Stats:       p.stats,
			memo:        p.memo,
			// Preorder (constraint-push) rules fire in their top-down constraint-only
			// pass — PushRequestedOrderingThrough{Sort,Filter,Select,...}Rule and
			// PushReferencedFields*Rule gate on IsConstraintOnly(). Without this the
			// entire Java-faithful ordering/referenced-fields constraint-propagation
			// phase is wired but inert, so a requested ordering never reaches the scan
			// and sort elimination through a residual filter never fires (RFC-076 3a).
			constraintOnly: isPreOrderRule(t.Rule),
		}
		t.Rule.OnMatch(call)

		// Handle yields: insert into FinalMembers and push explore+optimize
		// for genuinely new expressions. Skip re-exploration for
		// FinalizeExpressionsRule yields (they're already-explored
		// exploratory members promoted to final).
		for _, y := range call.yielded {
			// Java's unconditional verifyChildrenMemoized, at the same point:
			// before the yielded expression enters the memo.
			if err := verifyChildrenMemoized(y, p.reach); err != nil {
				p.capErr = err
				return
			}
			t.Ref.InsertFinal(y)
			if !isAlreadyExploratoryMember(t.Ref, y) {
				// OptimizeInputs only for PHYSICAL yields — third of the three gated
				// rule-yield sites (with ExploreGroupTask + the TransformExprTask yield).
				// ImplementationRule yields are physical wrappers, so this is a no-op in
				// practice, but it makes the "OptimizeInputs only for plan expressions"
				// property explicit at this site rather than relying on the rule kind.
				// The 4th site (the swapped-quantifier impl yield below) is
				// intentionally NOT gated — it is load-bearing, not redundant.
				if isPhysical(y) {
					p.push(&OptimizeInputsTask{Phase: t.Phase, Ref: t.Ref, Expr: y})
				}
				p.push(&ExploreExprTask{Phase: t.Phase, Ref: t.Ref, Expr: y})
			}
		}

		// Handle constraint pushes: re-explore affected child References.
		if call.Constraints != nil {
			for _, childRef := range call.constraintPushedRefs {
				p.push(&ExploreGroupTask{Phase: t.Phase, Ref: childRef})
			}
		}
	}

	// Also fire on swapped quantifiers for join commutativity.
	// The swapped expression is NOT a member of the Reference, so
	// it must bypass the ContainsExactly guard. Fire the rule
	// directly on the swapped expression.
	if sel, ok := t.Expr.(*expressions.SelectExpression); ok && sel.ChildrenAsSet() {
		qs := sel.GetQuantifiers()
		if len(qs) >= 2 && sel.GetJoinType() != expressions.JoinLeftOuter &&
			qs[0].Kind() == expressions.QuantifierForEach &&
			qs[1].Kind() == expressions.QuantifierForEach {
			swapped := sel.WithSwappedQuantifiers()
			swapBindings := t.Rule.Matcher().BindMatches(matching.NewBindings(), swapped)
			// Same rule call as the primary bind site above: the match cap
			// counts cumulatively across both binding streams.
			numMatches += len(swapBindings)
			if p.MaxNumMatchesPerRuleCall > 0 && numMatches > p.MaxNumMatchesPerRuleCall {
				p.capErr = ErrPlannerRuleMatchCapHit
				return
			}
			for _, b := range swapBindings {
				call := &ImplementationRuleCall{
					Bindings:    b,
					Reference:   t.Ref,
					Context:     p.ctx,
					Constraints: p.constraintMap,
					Stats:       p.stats,
					memo:        p.memo,
				}
				t.Rule.OnMatch(call)
				for _, y := range call.yielded {
					if err := verifyChildrenMemoized(y, p.reach); err != nil {
						p.capErr = err
						return
					}
					t.Ref.InsertFinal(y)
					if !isAlreadyExploratoryMember(t.Ref, y) {
						// NOTE: this 4th OptimizeInputs site — the
						// swapped-quantifier impl yield — is INTENTIONALLY NOT gated to
						// isPhysical. Unlike the other three, it is load-bearing, not a
						// no-op: gating it defers finalization in a way that breaks
						// TestFDB_ArrayUnnestOrdinality (HAVING on a shadowed grouped
						// unnest key). The B1 correlated-leg invariant doesn't need it —
						// the swapped path is join-commutativity over already-explored
						// members, not the correlated-SUBSEL yield path —
						// so the three gated sites are the complete set for the invariant.
						// A correlated INNER leg CAN reach this swap (ChildrenAsSet is true
						// for JoinInner, no correlation gate on the swap), but that is
						// HARMLESS: residual 0-row safety for any correlated leg is held
						// DOWNSTREAM and independently of which site drives the optimize —
						// by compensationSafeForYield's outer-correlation guard — not by B1's
						// gating. B1 only removes a premature standalone prune (the
						// :248 path above); it was never the sole 0-row guarantee.
						p.push(&OptimizeInputsTask{Phase: t.Phase, Ref: t.Ref, Expr: y})
						p.push(&ExploreExprTask{Phase: t.Phase, Ref: t.Ref, Expr: y})
					}
				}
			}
		}
	}
}

// OptimizeGroupTask picks the best final expression and prunes losers.
// Mirrors Java's CascadesPlanner.OptimizeGroup.
type OptimizeGroupTask struct {
	Phase PlannerPhase
	Ref   *expressions.Reference
}

func (t *OptimizeGroupTask) Run(p *Planner) {
	if t.Ref == nil {
		return
	}

	// Compute plan properties from final members so ToPlanPartitions
	// can find physical plans during PLANNING.
	if t.Phase == PhasePlanning {
		computeRefPlanProperties(t.Ref)
	}

	costModel := p.costModelForPhase(t.Phase)

	var bestFinal expressions.RelationalExpression
	for _, m := range t.Ref.FinalMembers() {
		if bestFinal == nil || costModel(m, bestFinal) {
			bestFinal = m
		}
	}

	if bestFinal == nil {
		// No finals at all — nothing to prune to and no winner to stamp.
		return
	}

	// Winner-per-(group, properties) retention (Graefe 1995 §2): pruning to
	// the single overall cost winner would destroy a costlier-but-ORDERED
	// final that a pushed RequestedOrderingConstraint asked this group for —
	// before the parent's ordered lookup (bestSatisfyingMember) or the
	// extraction elision seam can choose it, silently forcing an avoidable
	// enforcer sort. Java parents don't hit this because they bake concrete
	// child plans at rule time (memoizePlan); Go wrappers range over child
	// References resolved at lookup/extraction, so the group must retain,
	// per requested ordering, the cheapest final satisfying it. Orderings
	// nothing requested stay dead weight and are pruned with the losers.
	keep := map[expressions.RelationalExpression]struct{}{bestFinal: {}}
	if t.Phase == PhasePlanning {
		if ros, ok := Get(p.constraintMap, t.Ref, RequestedOrderingConstraintKey); ok {
			tieBrokenLess := lessWithHashTieBreak(costModel)
			for _, ro := range ros {
				if ro == nil || ro.IsPreserve() {
					continue
				}
				var best expressions.RelationalExpression
				for _, m := range t.Ref.FinalMembers() {
					if !memberSatisfiesOrdering(m, ro) {
						continue
					}
					if best == nil || tieBrokenLess(m, best) {
						best = m
					}
				}
				if best != nil {
					keep[best] = struct{}{}
				}
			}
		}
	}
	t.Ref.PruneToSet(keep)
	t.Ref.SetWinner(bestFinal)
}

// OptimizeInputsTask pushes OptimizeGroup for each child quantifier.
// Mirrors Java's CascadesPlanner.OptimizeInputs.
type OptimizeInputsTask struct {
	Phase PlannerPhase
	Ref   *expressions.Reference
	Expr  expressions.RelationalExpression
}

func (t *OptimizeInputsTask) Run(p *Planner) {
	if t.Expr == nil {
		return
	}
	// Identity guard (Java CascadesPlanner.OptimizeInputs:
	// `if (!group.containsExactly(expression)) return;`): an expression
	// pruned OUT of its group between task push and pop is dead — it
	// must not drive child-group pruning on behalf of a plan that lost.
	// With dual insertion retired (WS-P stage (b)) a physical yield's
	// only home is the final set, so Java's containsExactly and the
	// former finals-only check coincide — the compensation reverts.
	if t.Ref != nil && !t.Ref.ContainsExactly(t.Expr) {
		return
	}
	for _, q := range t.Expr.GetQuantifiers() {
		childRef := q.GetRangesOver()
		if childRef == nil {
			continue
		}
		p.push(&OptimizeGroupTask{Phase: t.Phase, Ref: childRef})
		p.push(&ExploreGroupTask{Phase: t.Phase, Ref: childRef})
	}
}

// isFinalMember checks if expr is already in the Reference's final members.
// isExploratoryMember reports pointer-identity membership in the
// EXPLORATORY set only (ContainsExactly admits finals too).
func isExploratoryMember(ref *expressions.Reference, expr expressions.RelationalExpression) bool {
	for _, m := range ref.Members() {
		if m == expr {
			return true
		}
	}
	return false
}

func isFinalMember(ref *expressions.Reference, expr expressions.RelationalExpression) bool {
	for _, m := range ref.FinalMembers() {
		if m == expr {
			return true
		}
	}
	return false
}

// isAlreadyExploratoryMember checks if expr is already in the Reference's
// exploratory members (by pointer identity). Used to skip re-exploration
// of FinalizeExpressionsRule yields.
func isAlreadyExploratoryMember(ref *expressions.Reference, expr expressions.RelationalExpression) bool {
	for _, m := range ref.Members() {
		if m == expr {
			return true
		}
	}
	return false
}

// isPreOrderRule returns true for rules that should fire BEFORE child
// exploration (constraint-push rules). These are pushed last on LIFO
// so they execute first.
func isPreOrderRule(rule ImplementationRule) bool {
	type preOrder interface {
		IsPreOrder() bool
	}
	if po, ok := rule.(preOrder); ok {
		return po.IsPreOrder()
	}
	return false
}
