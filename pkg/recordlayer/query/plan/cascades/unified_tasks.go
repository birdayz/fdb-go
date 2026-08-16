package cascades

import (
	"context"
	"fmt"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// InitiatePlannerPhaseTask starts a planner phase. Pushed once per phase.
// LIFO ordering ensures: ExploreGroup fires first, then OptimizeGroup,
// then the next phase's InitiatePlannerPhaseTask.
// Mirrors Java's CascadesPlanner.InitiatePlannerPhase.
type InitiatePlannerPhaseTask struct {
	Phase   PlannerPhase
	RootRef *expressions.Reference
}

func (t *InitiatePlannerPhaseTask) Run(ctx context.Context, p *Planner) {
	if ctx.Err() != nil {
		return
	}
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
	// forceNew keeps an explicit constraint/member-growth re-arm from being
	// absorbed by older pending work. Stage continuations are distinguished by
	// their captured stage; child dependency batches use the stack-ordering
	// helper below instead.
	forceNew bool

	// pendingKey is the canonical group identity captured when the planner
	// enqueues this task. A group can forward to another group while the task
	// is waiting on the LIFO stack, so pop must remove the captured key rather
	// than recomputing it from Ref.
	pendingKey exploreGroupTaskKey
	pending    bool
}

func (t *ExploreGroupTask) Run(ctx context.Context, p *Planner) {
	if ctx.Err() != nil || t.Ref == nil {
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
			//
			// The promote-and-clear applies to PHYSICAL finals too (a
			// mid-PLANNING MemoizeFinalExpression mint visited later): a
			// stage-preserving variant that kept plans as finals was
			// tried while chasing the LEFT-box + unnest + EXISTS no-plan
			// and reverted — the extra surviving finals flipped unrelated
			// cost races (in_list_index_plan lost its InUnion to a
			// sort-wrapped InJoin), and the actual no-plan root cause was
			// the existential rule consuming a winner without the
			// required seed-shaped result value (see the property-driven
			// reselection in rule_implement_nested_loop_join.go).
			t.Ref.AdvancePlannerStage(targetStage)
		} else {
			// No finals to promote — keep the exploratory members, but
			// still reset the PER-STAGE exploration bookkeeping so those
			// members re-explore in the new stage. A group whose logical
			// members survived REWRITING (e.g. an unfinalized merged union
			// leg) would otherwise carry its "explorationDone" state across
			// the boundary, never fire its implement rules in PLANNING, and
			// leave its parent with no physical child. Merely changing the
			// stage without this reset was the RFC-182 asymmetric-union
			// no-plan bug.
			t.Ref.AdvanceStagePreservingMembers(targetStage)
		}
	}

	// PinnedFinalOf is the planner's pinned physical-spine shape: a StagePlanned
	// reference with no exploratory members and exactly one physical final. It
	// represents a parent that was deliberately constructed over that concrete
	// child. Re-exploring the singleton would let physical transformation rules
	// add competitors and a child-local winner would then mutate the parent onto
	// a different plan — precisely undoing the restriction.
	//
	// Validate every exact ordinal requirement before accepting the pin. A pin
	// is a choice, not a license to bypass layout compatibility.
	if t.Ref.IsPinnedFinal() && targetStage == expressions.StagePlanned && len(t.Ref.Members()) == 0 {
		finals := t.Ref.FinalMembers()
		if len(finals) == 1 && isPhysical(finals[0]) {
			pinned := finals[0]
			if requirements, ok := Get(p.constraintMap, t.Ref, OrdinalLayoutConstraintKey); ok {
				for _, requirement := range requirements {
					compatible, err := memberSatisfiesOrdinalRequirement(pinned, requirement)
					if err != nil {
						p.capErr = fmt.Errorf("pinned child ordinal layout: %w", err)
						return
					}
					if !compatible {
						p.capErr = fmt.Errorf("pinned child ordinal layout: selected final is incompatible")
						return
					}
				}
			}
			t.Ref.SetWinner(pinned)
			computeRefPlanProperties(t.Ref)
			t.Ref.ConstraintsMap().SetExplored()
			return
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
		if ctx.Err() != nil {
			return
		}
		if isExploratoryMember(t.Ref, expr) {
			continue // also an exploratory member — the member loop owns it
		}
		// OptimizeInputs routing per phase (Java ExploreGroup routes
		// EVERY final through exploreExpressionAndOptimizeInputs):
		//   - REWRITING (WS-P stage (c)): finals are the canonical
		//     LOGICAL forms FinalizeExpressionsRule promoted;
		//     OptimizeInputs → OptimizeGroup prunes groups whose finals
		//     existed when this routing pass ran. This is TIMING-DEPENDENT
		//     coverage, not Java's universal property: finals promoted in a
		//     group's last exploration round get no OptimizeInputs pass, so
		//     the stage boundary crosses those groups UN-pruned with their
		//     full canonical set (see the stage-boundary comment above —
		//     the universal prune was tried and reverted). Cost derivation
		//     is insulated from the multi-final state by the RFC-186
		//     DESIGNATED final (designated_final.go, the virtual prune).
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
		if ctx.Err() != nil {
			return
		}
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
		// OptimizeInputs exactly like Java's getFinalExpressions split.
		// The resulting child prunes are TIMING-DEPENDENT (last-round
		// promotions get no pass; see the finals-routing comment above);
		// cost derivation is insulated by the RFC-186 designated final.
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

	// See ExploreGroupTask.pendingKey. Expression tasks need the same captured
	// identity because memo integration can forward Ref before this task pops.
	pendingKey exploreExprTaskKey
	pending    bool
}

func (t *ExploreExprTask) Run(ctx context.Context, p *Planner) {
	if ctx.Err() != nil || t.Ref == nil || t.Expr == nil {
		return
	}

	// Root-operator rule selection (Java AbstractRuleSet.getRules): only
	// rules whose matcher can possibly match t.Expr's concrete type get
	// transform tasks — the rest would pop, type-assert, and fail anyway.
	exprIdx, implIdx := p.ruleIndexesForPhase(t.Phase)
	exprRules := exprIdx.rulesFor(t.Expr)
	implRules := implIdx.rulesFor(t.Expr)
	// Everything pushed before child exploration is a dependent batch: it may
	// only run after every child group has reached its pending exploration.
	dependentFloor := len(p.stack)

	// 1. Push match-partition rules (fire LAST — deepest on LIFO).
	// Data access generation from PartialMatches.
	if t.Phase == PhasePlanning {
		p.pushDataAccessTasks(t.Ref, t.Expr)
	}

	// 2. Push non-preorder implementation rules.
	// Skip FinalizeExpressionsRule for expressions already in finals.
	for i := len(implRules) - 1; i >= 0; i-- {
		if ctx.Err() != nil {
			return
		}
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
		if ctx.Err() != nil {
			return
		}
		p.push(&TransformExprTask{Phase: t.Phase, Ref: t.Ref, Expr: t.Expr, Rule: exprRules[i]})
	}

	// 4. Schedule every child exploration before the dependent batch. When a
	// matching task is already pending deeper in the LIFO stack, move the batch
	// behind that task rather than either running the parent early or minting a
	// duplicate exploration task.
	childRefs := make([]*expressions.Reference, 0, len(t.Expr.GetQuantifiers()))
	for _, q := range t.Expr.GetQuantifiers() {
		if ctx.Err() != nil {
			return
		}
		if childRef := q.GetRangesOver(); childRef != nil {
			childRefs = append(childRefs, childRef)
		}
	}
	p.scheduleExploreGroupsBeforeBatch(t.Phase, childRefs, dependentFloor)

	// 5. Push preorder implementation rules (fire FIRST — topmost on LIFO).
	for i := len(implRules) - 1; i >= 0; i-- {
		if ctx.Err() != nil {
			return
		}
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

func (t *TransformExprTask) Run(ctx context.Context, p *Planner) {
	if ctx.Err() != nil || t.Ref == nil || t.Expr == nil || t.Rule == nil {
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
	// Java's per-rule-call match cap counts ONE stream per rule invocation
	// (CascadesPlanner.execute: a single numMatches over bindMatches, which
	// enumerates quantifier permutations inside the same stream). The swapped
	// bind below is part of THIS rule call, so it shares the counter.
	numMatches := 0
	fireExprRule := func(expr expressions.RelationalExpression) {
		if ctx.Err() != nil {
			return
		}
		bindings := t.Rule.Matcher().BindMatches(matching.NewBindings(), expr)
		if ctx.Err() != nil {
			return
		}
		numMatches += len(bindings)
		if p.MaxNumMatchesPerRuleCall > 0 && numMatches > p.MaxNumMatchesPerRuleCall {
			p.capErr = newRuleMatchCapError(p.MaxNumMatchesPerRuleCall, numMatches)
			return
		}
		for _, b := range bindings {
			if ctx.Err() != nil {
				return
			}
			call := &ExpressionRuleCall{
				Bindings:    b,
				Reference:   t.Ref,
				Context:     p.ctx,
				RunContext:  ctx,
				Constraints: p.constraintMap,
				Stats:       p.stats,
				memo:        p.memo,
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
			if ctx.Err() != nil {
				return
			}
			if err := call.Err(); err != nil {
				p.capErr = err
				return
			}
			// The rule succeeded, so its staged child inserts are publishable.
			// Before the yields, so a parent lands over complete children.
			call.CommitStagedInserts()

			yielded := call.Yielded()
			inserted := make([]bool, len(yielded))
			// Validate the complete invocation-local batch before publishing
			// any member. A later invalid yield must not leave an earlier
			// expression inserted into the memo.
			if t.Phase == PhasePlanning {
				for _, newExpr := range yielded {
					if err := verifyChildrenMemoized(newExpr, p.reach); err != nil {
						p.capErr = err
						return
					}
				}
			}

			// Exact-result admission and memo equality are prepared for the WHOLE
			// batch; apply is method-free and has no fallible operation after its
			// first write.
			if len(yielded) > 0 {
				set := expressions.ReferenceExploratoryMembers
				if t.Phase == PhasePlanning {
					set = expressions.ReferenceFinalMembers
				}
				intents := make([]referenceMemberIntent, len(yielded))
				for i, newExpr := range yielded {
					intents[i] = referenceMemberIntent{set: set, expression: newExpr}
				}
				batch, err := prepareReferenceMemberBatch(t.Ref, intents)
				if err != nil {
					p.capErr = err
					return
				}
				if err := batch.commit(); err != nil {
					p.capErr = err
					return
				}
				copy(inserted, batch.inserted)
			}
			if t.Phase != PhasePlanning && p.memo != nil {
				for _, newExpr := range yielded {
					p.memo.Integrate(t.Ref, newExpr)
				}
			}
			if t.Phase == PhasePlanning && len(t.Ref.GetAllPartialMatches()) > matchesBefore {
				p.pushDataAccessTasks(t.Ref, t.Expr)
			}

			for i, newExpr := range yielded {
				if ctx.Err() != nil {
					return
				}
				if !inserted[i] {
					continue
				}
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

func (t *TransformImplTask) Run(ctx context.Context, p *Planner) {
	if ctx.Err() != nil || t.Ref == nil || t.Expr == nil || t.Rule == nil {
		return
	}
	if !t.Ref.ContainsExactly(t.Expr) {
		return
	}
	bindings := t.Rule.Matcher().BindMatches(matching.NewBindings(), t.Expr)
	if ctx.Err() != nil {
		return
	}
	// Java CascadesPlanner.isMaxNumMatchesPerRuleCallExceeded: one rule
	// invocation producing more matches than the bound is a complexity
	// blow-up; throw (here: capErr — tasks have no error channel). The
	// counter is ONE stream per rule call in Java (quantifier permutations
	// included), so the swapped bind below adds to it rather than resetting.
	numMatches := len(bindings)
	if p.MaxNumMatchesPerRuleCall > 0 && numMatches > p.MaxNumMatchesPerRuleCall {
		p.capErr = newRuleMatchCapError(p.MaxNumMatchesPerRuleCall, numMatches)
		return
	}
	for _, b := range bindings {
		if ctx.Err() != nil {
			return
		}
		call := &ImplementationRuleCall{
			Bindings:    b,
			Reference:   t.Ref,
			Context:     p.ctx,
			RunContext:  ctx,
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
		if ctx.Err() != nil {
			return
		}
		if err := call.Err(); err != nil {
			p.capErr = err
			return
		}

		// Preflight every yield before applying either member insertions or
		// requested-ordering constraints. This makes the rule call atomic on
		// both an explicit Fail and an invalid later yield.
		for _, y := range call.yielded {
			if err := verifyChildrenMemoized(y, p.reach); err != nil {
				p.capErr = err
				return
			}
		}
		// The batch preflighted, so held child inserts are publishable.
		call.CommitStagedInserts()
		if len(call.yielded) > 0 {
			intents := make([]referenceMemberIntent, len(call.yielded))
			for i, y := range call.yielded {
				intents[i] = referenceMemberIntent{set: expressions.ReferenceFinalMembers, expression: y}
			}
			batch, err := prepareReferenceMemberBatch(t.Ref, intents)
			if err != nil {
				p.capErr = err
				return
			}
			if err := batch.commit(); err != nil {
				p.capErr = err
				return
			}
		}
		call.applyPendingConstraints()

		// Handle yields: insert into FinalMembers and push explore+optimize
		// for genuinely new expressions. Skip re-exploration for
		// FinalizeExpressionsRule yields (they're already-explored
		// exploratory members promoted to final).
		for _, y := range call.yielded {
			if ctx.Err() != nil {
				return
			}
			// InsertFinal only — deliberately NO re-prune of a stamped group
			// on late final growth. A re-push-OptimizeGroup-on-growth hook
			// was tried here and REVERTED: re-pruning leaves ONE final where
			// the stage boundary previously carried both the old winner and
			// the late newcomer, and PLANNING cannot re-derive the pruned
			// alternative (the same failure mode as the reverted universal
			// boundary prune — the DistinctOverUnionAll dedup collapsed to a
			// bare scan). Costing soundness does not need the prune: the
			// RFC-186 designation re-computes on growth (the insert bumps
			// the finals generation), so the virtual prune stays fresh while
			// the member set keeps every alternative PLANNING needs.
			if call.indexYieldedInMemo && p.memo != nil {
				p.memo.AddExpression(t.Ref, y)
			}
			if !isExploratoryMember(t.Ref, y) {
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
				if ctx.Err() != nil {
					return
				}
				p.push(&ExploreGroupTask{Phase: t.Phase, Ref: childRef, forceNew: true})
			}
		}
	}

	// Also fire on swapped quantifiers for join commutativity.
	// The swapped expression is NOT a member of the Reference, so
	// it must bypass the ContainsExactly guard. Fire the rule
	// directly on the swapped expression.
	if ctx.Err() == nil {
		if sel, ok := t.Expr.(*expressions.SelectExpression); ok && sel.ChildrenAsSet() {
			qs := sel.GetQuantifiers()
			if len(qs) >= 2 && sel.GetJoinType() != expressions.JoinLeftOuter &&
				qs[0].Kind() == expressions.QuantifierForEach &&
				qs[1].Kind() == expressions.QuantifierForEach {
				swapped := sel.WithSwappedQuantifiers()
				swapBindings := t.Rule.Matcher().BindMatches(matching.NewBindings(), swapped)
				if ctx.Err() != nil {
					return
				}
				// Same rule call as the primary bind site above: the match cap
				// counts cumulatively across both binding streams.
				numMatches += len(swapBindings)
				if p.MaxNumMatchesPerRuleCall > 0 && numMatches > p.MaxNumMatchesPerRuleCall {
					p.capErr = newRuleMatchCapError(p.MaxNumMatchesPerRuleCall, numMatches)
					return
				}
				for _, b := range swapBindings {
					if ctx.Err() != nil {
						return
					}
					call := &ImplementationRuleCall{
						Bindings:    b,
						Reference:   t.Ref,
						Context:     p.ctx,
						RunContext:  ctx,
						Constraints: p.constraintMap,
						Stats:       p.stats,
						memo:        p.memo,
					}
					t.Rule.OnMatch(call)
					if ctx.Err() != nil {
						return
					}
					if err := call.Err(); err != nil {
						p.capErr = err
						return
					}
					for _, y := range call.yielded {
						if err := verifyChildrenMemoized(y, p.reach); err != nil {
							p.capErr = err
							return
						}
					}
					// The batch preflighted, so held child inserts are publishable.
					call.CommitStagedInserts()
					if len(call.yielded) > 0 {
						intents := make([]referenceMemberIntent, len(call.yielded))
						for i, y := range call.yielded {
							intents[i] = referenceMemberIntent{set: expressions.ReferenceFinalMembers, expression: y}
						}
						batch, err := prepareReferenceMemberBatch(t.Ref, intents)
						if err != nil {
							p.capErr = err
							return
						}
						if err := batch.commit(); err != nil {
							p.capErr = err
							return
						}
					}
					call.applyPendingConstraints()
					for _, y := range call.yielded {
						if ctx.Err() != nil {
							return
						}
						if call.indexYieldedInMemo && p.memo != nil {
							p.memo.AddExpression(t.Ref, y)
						}
						if !isExploratoryMember(t.Ref, y) {
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
					if call.Constraints != nil {
						for _, childRef := range call.constraintPushedRefs {
							if ctx.Err() != nil {
								return
							}
							p.push(&ExploreGroupTask{Phase: t.Phase, Ref: childRef, forceNew: true})
						}
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

	// Captured at enqueue time for the same forwarding-safe pending-only
	// coalescing used by ExploreGroupTask. A single eventual optimizer observes
	// the group's latest final set and accumulated constraints when it pops.
	pendingKey exploreGroupTaskKey
	pending    bool
}

func (t *OptimizeGroupTask) Run(ctx context.Context, p *Planner) {
	if ctx.Err() != nil || t.Ref == nil {
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
		if ctx.Err() != nil {
			return
		}
		if bestFinal == nil || costModel(m, bestFinal) {
			bestFinal = m
		}
	}

	if bestFinal == nil {
		// No finals at all — nothing to prune to and no winner to stamp.
		return
	}

	// Winner-per-(group, properties) retention (Graefe 1995 §2): pruning to
	// the single overall cost winner would destroy a costlier final required
	// by a parent — either an ordered provider needed to avoid an enforcer
	// sort, or an ordinal layout against which the parent's retained Values
	// were finalized. Java parents usually bake concrete child plans at rule
	// time (memoizePlan); Go plans range over child References resolved at
	// lookup/extraction, so the group retains the cheapest final satisfying
	// each pushed property in addition to its global winner.
	//
	// DistinctRecords and StoredRecord are also parent-visible interesting
	// properties even when no explicit constraint was pushed. A parent rule can
	// make a locally costlier partition member globally cheaper: most notably a
	// Projection can push through Fetch(Covering(Index)) and remove the Fetch,
	// while it cannot perform that rewrite over the locally cheaper fetching
	// Index member. Retaining only the global child winner therefore prevents the
	// cheaper parent tree from ever being constructed. Keep the cheapest member
	// of every (distinct, stored) class; ordering is deliberately excluded here
	// and remains demand-driven below, so an unrequested sort is still pruned.
	keep := map[expressions.RelationalExpression]struct{}{bestFinal: {}}
	if t.Phase == PhasePlanning {
		type nonOrderingPartition struct {
			distinct bool
			stored   bool
		}
		tieBrokenLess := lessWithHashTieBreak(costModel)
		partitionWinners := make(map[nonOrderingPartition]expressions.RelationalExpression)
		if planProperties := GetRefPlanPropertiesMap(t.Ref); planProperties != nil {
			for _, member := range t.Ref.FinalMembers() {
				memberProperties := planProperties.GetProperties(member)
				if memberProperties == nil {
					continue
				}
				partition := nonOrderingPartition{
					distinct: memberProperties.GetBool(properties.PropDistinctRecords),
					stored:   memberProperties.GetBool(properties.PropStoredRecord),
				}
				winner := partitionWinners[partition]
				if winner == nil || tieBrokenLess(member, winner) {
					partitionWinners[partition] = member
				}
			}
		}
		for _, winner := range partitionWinners {
			keep[winner] = struct{}{}
		}

		if ros, ok := Get(p.constraintMap, t.Ref, RequestedOrderingConstraintKey); ok {
			for _, ro := range ros {
				if ctx.Err() != nil {
					return
				}
				if ro == nil || ro.IsPreserve() {
					continue
				}
				var best expressions.RelationalExpression
				for _, m := range t.Ref.FinalMembers() {
					if ctx.Err() != nil {
						return
					}
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
		if requirements, ok := Get(p.constraintMap, t.Ref, OrdinalLayoutConstraintKey); ok {
			for _, requirement := range requirements {
				if ctx.Err() != nil {
					return
				}
				best, err := bestOrdinalCompatiblePhysicalMemberAmong(
					t.Ref.FinalMembers(), requirement, tieBrokenLess)
				if err != nil {
					p.capErr = fmt.Errorf("ordinal layout winner for child group: %w", err)
					return
				}
				if best == nil {
					p.capErr = fmt.Errorf("ordinal layout winner for child group: no compatible final member remains")
					return
				}
				keep[best] = struct{}{}
			}
		}
	}
	// Coherence is checked BEFORE the prune: the designation must rank the
	// SAME multi-final candidate set the compare loop just ranked — after
	// PruneToSet the group holds only {bestFinal} and the generation bump
	// forces a recompute, so a post-prune check matches by construction and
	// can never report the staleness/drift it exists to canary.
	if t.Phase == PhaseRewriting {
		p.checkRewritingCoherence(t.Ref, bestFinal)
	}
	t.Ref.PruneToSet(keep)
	t.Ref.SetWinner(bestFinal)
}

// checkRewritingCoherence is the RFC-186 coherence instrument: the winner a
// REWRITING OptimizeGroup is about to stamp and the designation cost
// properties derive through must be the SAME expression — both come from
// the same comparator (the planner's designation scope) ranking the same
// pre-prune candidate set, so a mismatch means the designation cache went
// stale (an entry surviving a mutation at an unbumped generation) or the
// comparators drifted (a compare path bypassing the scope), and REWRITING
// costing is again history-dependent. Extracted so the detection wiring is
// testable in isolation.
func (p *Planner) checkRewritingCoherence(ref *expressions.Reference, bestFinal expressions.RelationalExpression) {
	if !p.verifyRewritingCoherence || p.dscope == nil || ref == nil {
		return
	}
	if designated := p.dscope.designated(ref, nil); designated != bestFinal {
		p.rewritingCoherenceViolations = append(p.rewritingCoherenceViolations,
			fmt.Sprintf("group winner %T is not the designated final %T (comparator/designation divergence)",
				bestFinal, designated))
	}
}

// OptimizeInputsTask pushes OptimizeGroup for each child quantifier.
// Mirrors Java's CascadesPlanner.OptimizeInputs.
type OptimizeInputsTask struct {
	Phase PlannerPhase
	Ref   *expressions.Reference
	Expr  expressions.RelationalExpression
}

func (t *OptimizeInputsTask) Run(ctx context.Context, p *Planner) {
	if ctx.Err() != nil || t.Expr == nil {
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
	if t.Phase == PhasePlanning && t.Ref != nil {
		// OptimizeInputs tasks for sibling physical alternatives are popped in
		// LIFO order. Accumulate every still-live sibling's positional
		// requirements before the first one is allowed to schedule a child
		// prune; otherwise the last-pushed parent could prune away a layout
		// required by an earlier sibling whose task has not run yet.
		if err := pushOrdinalInputRequirementsForMembers(p.constraintMap, t.Ref.AllMembers()); err != nil {
			p.capErr = err
			return
		}
	}
	requirements, err := ordinalInputRequirementsOf(t.Expr)
	if err != nil {
		p.capErr = err
		return
	}
	dependentFloor := len(p.stack)
	childRefs := make([]*expressions.Reference, 0, len(t.Expr.GetQuantifiers()))
	for i, q := range t.Expr.GetQuantifiers() {
		if ctx.Err() != nil {
			return
		}
		childRef := q.GetRangesOver()
		if childRef == nil {
			continue
		}
		if t.Phase == PhasePlanning && len(requirements) != 0 {
			// The positional vector was validated against quantifier arity by
			// ordinalInputRequirementsOf. Set combines requirements from
			// multiple finalized parents sharing this child group; the
			// immediately scheduled Explore/Optimize pair observes the grown
			// constraint before it can prune the compatible alternative.
			Set(p.constraintMap, childRef, OrdinalLayoutConstraintKey,
				[]plans.OrdinalLayoutRequirement{requirements[i]})
		}
		p.push(&OptimizeGroupTask{Phase: t.Phase, Ref: childRef})
		childRefs = append(childRefs, childRef)
	}
	p.scheduleExploreGroupsBeforeBatch(t.Phase, childRefs, dependentFloor)
}

// pushOrdinalInputRequirementsForMembers performs the group-local prepass used
// by OptimizeInputs. It is deliberately limited to members already live in the
// same parent group; requirements from unrelated groups still flow only when
// their own physical parent is optimized. Repeated members and repeated exact
// requirements are harmless because the constraint lattice subsumes them.
func pushOrdinalInputRequirementsForMembers(
	constraints *ConstraintMap,
	members []expressions.RelationalExpression,
) error {
	seen := make(map[expressions.RelationalExpression]struct{}, len(members))
	for _, member := range members {
		if member == nil {
			continue
		}
		if _, duplicate := seen[member]; duplicate {
			continue
		}
		seen[member] = struct{}{}
		requirements, err := ordinalInputRequirementsOf(member)
		if err != nil {
			return err
		}
		if len(requirements) == 0 {
			continue
		}
		for i, q := range member.GetQuantifiers() {
			childRef := q.GetRangesOver()
			if childRef == nil {
				continue
			}
			Set(constraints, childRef, OrdinalLayoutConstraintKey,
				[]plans.OrdinalLayoutRequirement{requirements[i]})
		}
	}
	return nil
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
