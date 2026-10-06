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

	// superseded marks a task queued for a group a PLANNING merge folded
	// away; the merge scheduled the exploration the fold needs.
	superseded bool
}

func (t *ExploreGroupTask) Run(ctx context.Context, p *Planner) {
	if ctx.Err() != nil || t.Ref == nil || t.superseded {
		return
	}

	targetStage := t.Phase.TargetStage()
	refStage := t.Ref.Stage()

	if targetStage != refStage {
		if targetStage.Precedes(refStage) {
			return
		}
		// Java Reference.advancePlannerStage: Verify(finalMembers.size() == 1).
		// REWRITING prunes every group it reaches to its one winning final
		// (OptimizeInputs → OptimizeGroup over each final's inputs, the physical
		// prune), so the group crosses as that one final, which becomes the
		// seed PLANNING explores. A group with no final is one REWRITING never
		// reached and one with several was never pruned: both are scheduling
		// defects, refused here rather than carried into PLANNING, where a
		// member no REWRITING rule ran on would compete. The promote-and-clear
		// applies to PHYSICAL finals too (a mid-PLANNING MemoizeFinalExpression
		// mint visited later).
		if finals := len(t.Ref.FinalMembers()); finals != 1 {
			crossing := &RewritingCrossingError{Finals: finals}
			for _, member := range t.Ref.Members() {
				crossing.Members = append(crossing.Members, fmt.Sprintf("%T", member))
			}
			p.capErr = crossing
			return
		}
		t.Ref.AdvancePlannerStage(targetStage)
	}

	if targetStage == expressions.StagePlanned {
		completed, err := p.completePinnedFinal(t.Ref)
		if err != nil {
			p.capErr = err
			return
		}
		if completed {
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
		// A task queued for a group since merged away does not own the
		// survivor's round in flight; committing it here would close that
		// round before its member tasks ran.
		if t.Ref.IsForwarded() && t.Ref.ConstraintsMap().IsExploring() {
			return
		}
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

	if completed, err := t.completeRulelessGroup(ctx, p); err != nil {
		p.capErr = err
		return
	} else if completed {
		t.Ref.StartExploration()
		t.Ref.CommitExploration()
		computeRefPlanProperties(t.Ref)
		return
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
		//   - REWRITING: finals are the canonical LOGICAL forms
		//     FinalizeExpressionsRule yielded; OptimizeInputs → OptimizeGroup
		//     prunes each final's input groups to their one winning final
		//     before the group itself is costed (Java's physical prune; the
		//     crossing above and the REWRITING comparator check it).
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
		p.push(&ExploreExprTask{Phase: t.Phase, Ref: t.Ref, Expr: expr, ReExplore: true})
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
		// REWRITING: a member that is ALSO a final — a leaf, which
		// FinalizeExpressionsRule yields as itself — routes through
		// OptimizeInputs exactly like Java's getFinalExpressions split.
		if (t.Phase == PhaseRewriting && isFinalMember(t.Ref, expr)) ||
			(t.Phase == PhasePlanning && isPhysical(expr)) {
			p.push(&OptimizeInputsTask{Phase: t.Phase, Ref: t.Ref, Expr: expr})
		}
		p.push(&ExploreExprTask{Phase: t.Phase, Ref: t.Ref, Expr: expr, ReExplore: true})
	}

	t.Ref.StartExploration()
}

// With no possible leaf transformation or input validation, the exploration
// epoch can finish immediately instead of scheduling an empty continuation.
func (t *ExploreGroupTask) isRulelessLeafGroup(p *Planner) bool {
	if t.Phase != PhasePlanning {
		return false
	}
	members := t.Ref.AllMembers()
	if len(members) == 0 {
		return false
	}
	for _, member := range members {
		if (&ExploreExprTask{Phase: t.Phase, Ref: t.Ref, Expr: member}).hasWork(p) ||
			(&OptimizeInputsTask{Phase: t.Phase, Ref: t.Ref, Expr: member}).hasWork() {
			return false
		}
	}
	return true
}

// No continuation is needed when neither the group nor its settled singleton
// inputs have a transformation or cost choice. Input contracts still apply.
func (t *ExploreGroupTask) completeRulelessGroup(ctx context.Context, p *Planner) (bool, error) {
	if t.isRulelessLeafGroup(p) {
		return true, nil
	}
	if t.Phase != PhasePlanning || len(t.Ref.GetAllPartialMatches()) != 0 {
		return false, nil
	}
	members := t.Ref.AllMembers()
	var inputs []*expressions.Reference
	for _, member := range members {
		if !isPhysical(member) {
			return false, nil
		}
		for _, q := range member.GetQuantifiers() {
			if ref := q.GetRangesOver(); ref != nil {
				inputs = append(inputs, ref)
			}
		}
	}
	if _, settled := p.settledSingletonInputs(inputs); !settled {
		return false, nil
	}
	exprIdx, implIdx := p.ruleIndexesForPhase(t.Phase)
	for _, member := range members {
		explore := &ExploreExprTask{Phase: t.Phase, Ref: t.Ref, Expr: member}
		if explore.hasRulesWithSettledInputs(p, exprIdx, implIdx.rulesFor(member)) {
			return false, nil
		}
	}
	if err := pushOrdinalInputRequirementsForMembers(p.constraintMap, members); err != nil {
		return false, err
	}
	return p.completeSingletonInputs(ctx, inputs)
}

// A pinned child is an exact selection, not a search space. Layout requirements
// still apply, including those pushed after its exploration completed.
func (p *Planner) completePinnedFinal(ref *expressions.Reference) (bool, error) {
	if ref.Stage() != expressions.StagePlanned || !ref.IsPinnedFinal() || len(ref.Members()) != 0 {
		return false, nil
	}
	finals := ref.FinalMembers()
	if len(finals) != 1 || !isPhysical(finals[0]) {
		return false, nil
	}
	pinned := finals[0]
	if requirements, ok := Get(p.constraintMap, ref, OrdinalLayoutConstraintKey); ok {
		for _, requirement := range requirements {
			compatible, err := memberSatisfiesOrdinalRequirement(pinned, requirement)
			if err != nil {
				return false, fmt.Errorf("pinned child ordinal layout: %w", err)
			}
			if !compatible {
				return false, fmt.Errorf("pinned child ordinal layout: selected final is incompatible")
			}
		}
	}
	ref.SetWinner(pinned)
	computeRefPlanProperties(ref)
	ref.ConstraintsMap().SetExplored()
	return true, nil
}

// ExploreExprTask pushes rule-transform tasks and child-exploration tasks
// for a single (group, expression) pair. Mirrors Java's AbstractExploreExpression.
type ExploreExprTask struct {
	ReExplore bool
	Phase     PlannerPhase
	Ref       *expressions.Reference
	Expr      expressions.RelationalExpression

	// See ExploreGroupTask.pendingKey. Expression tasks need the same captured
	// identity because memo integration can forward Ref before this task pops.
	pendingKey exploreExprTaskKey
	pending    bool
}

func (t *ExploreExprTask) hasWork(p *Planner) bool {
	if t.Phase != PhasePlanning || t.Ref == nil || t.Expr == nil {
		return true
	}
	// Match-producing transforms schedule their own partition work. Pre-existing
	// matches still need this visit, even when the leaf itself has no rules.
	if len(t.Ref.GetAllPartialMatches()) != 0 {
		return true
	}
	exprIdx, implIdx := p.ruleIndexesForPhase(t.Phase)
	implRules := implIdx.rulesFor(t.Expr)
	if len(t.Expr.GetQuantifiers()) != 0 {
		// Only exact selected children are stable at enqueue time; ordinary
		// singleton groups can still gain implementations before this task pops.
		for _, q := range t.Expr.GetQuantifiers() {
			child := q.GetRangesOver()
			if child == nil || !child.IsPinnedFinal() || len(child.Members()) != 0 {
				return true
			}
			if finals := child.FinalMembers(); len(finals) != 1 || !isPhysical(finals[0]) {
				return true
			}
		}
		if !t.childMatchesAreSettled(p, nil, implRules) {
			return true
		}
	}
	return t.hasRulesWithSettledInputs(p, exprIdx, implRules)
}

func (t *ExploreExprTask) hasRulesWithSettledInputs(p *Planner, exprIdx *ruleIndex[ExpressionRule], implRules []ImplementationRule) bool {
	for _, rule := range exprIdx.rulesFor(t.Expr) {
		if t.shouldPushExpressionRule(p, rule) {
			if _, intermediate := rule.(*MatchIntermediateRule); intermediate && !hasIntermediateMatchCandidate(t.Expr) {
				continue
			}
			return true
		}
	}
	for _, rule := range implRules {
		if t.shouldPushRule(rule) {
			if matcher, ok := rule.Matcher().(matching.InputPredicateMatcher); ok && !matcher.MatchesInputs(t.Expr) {
				continue
			}
			return true
		}
	}
	return false
}

func (t *ExploreExprTask) shouldPushExpressionRule(p *Planner, rule ExpressionRule) bool {
	if isPredicateUnionRule(rule) || !t.shouldPushRule(rule) {
		return false
	}
	if _, leaf := rule.(*MatchLeafRule); leaf {
		return p.canMatchLeaf(t.Expr)
	}
	return true
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
	if err := t.completeRulelessInput(ctx, p, implRules); err != nil {
		p.capErr = err
		return
	}
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
		if isPreOrderRule(rule) || isPrunedInputsRule(rule) || !t.shouldPushRule(rule) {
			continue
		}
		if _, ok := rule.(*FinalizeExpressionsRule); ok {
			if isFinalMember(t.Ref, t.Expr) {
				continue
			}
		}
		if matcher, ok := rule.Matcher().(matching.InputPredicateMatcher); ok &&
			!matcher.MatchesInputs(t.Expr) && t.inputsAreSettled(p, exprRules, implRules[:i], implRules) {
			continue
		}
		p.push(&TransformImplTask{Phase: t.Phase, Ref: t.Ref, Expr: t.Expr, Rule: rule})
	}

	// 3. Push non-preorder expression rules.
	for i := len(exprRules) - 1; i >= 0; i-- {
		if ctx.Err() != nil {
			return
		}
		if !t.shouldPushExpressionRule(p, exprRules[i]) {
			continue
		}
		if _, intermediate := exprRules[i].(*MatchIntermediateRule); intermediate &&
			t.childMatchesAreSettled(p, exprRules[:i], implRules) && !hasIntermediateMatchCandidate(t.Expr) {
			continue
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
	preorder := t.preOrderRules(p, implRules)
	for i := len(preorder) - 1; i >= 0; i-- {
		if ctx.Err() != nil {
			return
		}
		p.push(&TransformImplTask{Phase: t.Phase, Ref: t.Ref, Expr: t.Expr, Rule: preorder[i]})
	}
}

func (t *ExploreExprTask) preOrderRules(p *Planner, implRules []ImplementationRule) []ImplementationRule {
	var result []ImplementationRule
	for _, rule := range implRules {
		if !isPreOrderRule(rule) || isPrunedInputsRule(rule) || !t.shouldPushRule(rule) {
			continue
		}
		// Only an inert prefix is removable: an earlier rule can schedule
		// child work that changes a later rule's source or destination.
		if len(result) == 0 {
			if propagation, ok := rule.(constraintPropagationRule); ok && !propagation.hasConstraintEffect(p.constraintMap, t.Ref, t.Expr) {
				continue
			}
		}
		result = append(result, rule)
	}
	return result
}

// Complete a physical input batch's ruleless closure before admitting parent
// transforms. The proof is local to this visit; no ordinary group becomes pinned.
func (t *ExploreExprTask) completeRulelessInput(ctx context.Context, p *Planner, implRules []ImplementationRule) error {
	qs := t.Expr.GetQuantifiers()
	if t.Phase != PhasePlanning || !isPhysical(t.Expr) || len(qs) == 0 {
		return nil
	}
	if len(t.preOrderRules(p, implRules)) != 0 {
		return nil
	}
	exprIdx, implIdx := p.ruleIndexesForPhase(t.Phase)
	visiting := make(map[*expressions.Reference]bool)
	var order []*expressions.Reference
	var prove func(*expressions.Reference) bool
	proveInputs := func(qs []expressions.Quantifier) bool {
		seen := make(map[*expressions.Reference]bool, len(qs))
		var refs []*expressions.Reference
		for _, q := range qs {
			ref := q.GetRangesOver().Canonical()
			if !seen[ref] {
				seen[ref] = true
				refs = append(refs, ref)
			}
		}
		// Match the dependency scheduler's reverse first-occurrence order.
		for i := len(refs) - 1; i >= 0; i-- {
			if !prove(refs[i]) {
				return false
			}
		}
		return true
	}
	prove = func(ref *expressions.Reference) bool {
		if ref == nil || ctx.Err() != nil {
			return false
		}
		ref = ref.Canonical()
		if active, seen := visiting[ref]; seen {
			return !active
		}
		visiting[ref] = true
		finals := ref.FinalMembers()
		constraints := ref.ConstraintsMap()
		if ref.Stage() != expressions.StagePlanned || len(ref.Members()) != 0 || len(finals) != 1 ||
			!isPhysical(finals[0]) || constraints.IsExploring() {
			return false
		}
		if constraints.IsExplored() {
			visiting[ref] = false
			return true
		}
		if ref.IsPinnedFinal() || len(ref.GetAllPartialMatches()) != 0 {
			return false
		}
		expr := finals[0]
		if !proveInputs(expr.GetQuantifiers()) {
			return false
		}
		explore := &ExploreExprTask{Phase: t.Phase, Ref: ref, Expr: expr}
		if explore.hasRulesWithSettledInputs(p, exprIdx, implIdx.rulesFor(expr)) {
			return false
		}
		if _, err := ordinalInputRequirementsOf(expr); err != nil {
			return false
		}
		visiting[ref] = false
		order = append(order, ref)
		return true
	}
	if !proveInputs(qs) || p.hasPendingInputWork(visiting) {
		return nil
	}
	for _, ref := range order {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		group := &ExploreGroupTask{Phase: t.Phase, Ref: ref}
		completed, err := group.completeRulelessGroup(ctx, p)
		if err != nil || !completed {
			return err
		}
		ref.StartExploration()
		ref.CommitExploration()
		computeRefPlanProperties(ref)
	}
	return nil
}

// Only the first rule over already explored children can use their current
// matches: neither a preorder rule nor a pending child batch may run first.
func (t *ExploreExprTask) childMatchesAreSettled(p *Planner, earlier []ExpressionRule, implRules []ImplementationRule) bool {
	for _, rule := range earlier {
		if t.shouldPushExpressionRule(p, rule) {
			return false
		}
	}
	if len(t.preOrderRules(p, implRules)) != 0 {
		return false
	}
	for _, q := range t.Expr.GetQuantifiers() {
		ref := q.GetRangesOver()
		if ref == nil {
			continue
		}
		ref = ref.Canonical()
		constraints := ref.ConstraintsMap()
		if ref.Stage().Precedes(t.Phase.TargetStage()) || !constraints.IsExplored() || constraints.IsExploring() {
			return false
		}
		for pending := range p.pendingExploreGroups {
			if pending.phase == t.Phase && pending.ref.Canonical() == ref {
				return false
			}
		}
		for pending := range p.pendingDataAccess {
			if pending.Canonical() == ref {
				return false
			}
		}
	}
	return true
}

// Input patterns can be rejected before enqueueing only if every preceding
// transformation is absent, including matching and preorder constraint pushes.
func (t *ExploreExprTask) inputsAreSettled(p *Planner, exprRules []ExpressionRule, earlier, implRules []ImplementationRule) bool {
	if t.Phase != PhasePlanning || !t.childMatchesAreSettled(p, nil, implRules) {
		return false
	}
	for _, rule := range earlier {
		if !isPreOrderRule(rule) && t.shouldPushRule(rule) {
			return false
		}
	}
	for _, rule := range exprRules {
		if !t.shouldPushExpressionRule(p, rule) {
			continue
		}
		if _, intermediate := rule.(*MatchIntermediateRule); intermediate && !hasIntermediateMatchCandidate(t.Expr) {
			continue
		}
		return false
	}
	return true
}

func (t *ExploreExprTask) shouldPushRule(rule matcherHaver) bool {
	if matcher, ok := rule.Matcher().(matching.RootPredicateMatcher); ok && !matcher.MatchesRoot(t.Expr) {
		return false
	}
	if !t.ReExplore {
		return true
	}
	// Java's AbstractCascadesRule defaults to no constraint dependencies, so a
	// re-exploration re-queues a rule only when it declares a constraint pushed
	// since the group's last committed exploration (CascadesPlanner.java:956-970);
	// a re-arm (lastRearmTick) still re-queues every rule. Each rule that reads a
	// planner constraint declares it (ConstraintDependencies, RFC-257 WS-F D2).
	var deps []any
	if dependent, ok := rule.(interface{ ConstraintDependencies() []any }); ok {
		deps = dependent.ConstraintDependencies()
	}
	return !t.Ref.ConstraintsMap().IsExploredForAttributes(deps)
}

// TransformExprTask fires a single ExpressionRule on a (group, expression)
// pair. Yields go to exploratory members (ref.Insert).
// Mirrors Java's TransformExpression for ExplorationCascadesRule.
type TransformExprTask struct {
	Phase PlannerPhase
	Ref   *expressions.Reference
	Expr  expressions.RelationalExpression
	Rule  ExpressionRule
	// conditionalIndex is the inner rule a conditional Rule runs next.
	conditionalIndex int
}

// ConsumeMatchPartitionTask runs after the expression's matching transforms,
// so each candidate is consumed against the completed match partition.
type ConsumeMatchPartitionTask struct{ Ref *expressions.Reference }

func (t *ConsumeMatchPartitionTask) Run(ctx context.Context, p *Planner) {
	delete(p.pendingDataAccess, t.Ref)
	if ctx.Err() != nil || t.Ref == nil {
		return
	}
	p.consumeDataAccessPartitions(t.Ref)
}

// TransformMatchPartitionTask reacts to new matches and publishes logical alternatives.
type TransformMatchPartitionTask struct{ TransformExprTask }

func (t *TransformMatchPartitionTask) Run(ctx context.Context, p *Planner) {
	t.TransformExprTask.Run(ctx, p)
}

func (t *TransformExprTask) Run(ctx context.Context, p *Planner) {
	if ctx.Err() != nil || t.Ref == nil || t.Expr == nil || t.Rule == nil {
		return
	}
	cond, conditional := t.Rule.(*conditionalExpressionRule)
	if !conditional {
		t.runRule(ctx, p)
		return
	}
	// Java ConditionalTransformExpression: the next rule runs only when this
	// one made no progress.
	inner := *t
	inner.Rule = cond.rules[t.conditionalIndex]
	if !inner.runRule(ctx, p) && t.conditionalIndex+1 < len(cond.rules) && ctx.Err() == nil && p.capErr == nil {
		next := *t
		next.conditionalIndex++
		p.push(&next)
	}
}

// runRule applies the task's rule and reports progress as Java's
// executeRuleCall does: a new member inserted or a new partial match.
func (t *TransformExprTask) runRule(ctx context.Context, p *Planner) (progress bool) {
	if !t.Ref.ContainsExactly(t.Expr) {
		return
	}

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
			var batch *preparedReferenceBatch
			if len(yielded) > 0 {
				intents := make([]referenceMemberIntent, len(yielded))
				for i, newExpr := range yielded {
					// Planning exploration stays visible to logical memo lookup;
					// only physical implementations belong in the final lane.
					set := expressions.ReferenceExploratoryMembers
					if t.Phase == PhasePlanning && isPhysical(newExpr) {
						set = expressions.ReferenceFinalMembers
					}
					intents[i] = referenceMemberIntent{set: set, expression: newExpr}
				}
				prepared, err := prepareReferenceMemberBatch(t.Ref, intents)
				if err != nil {
					p.capErr = err
					return
				}
				batch = prepared
			}
			// AFTER every fallible step — the verify above AND the prepare — not
			// merely after Err. A clear Err says only that the rule BODY
			// succeeded; the batch can still be rejected here, and an insert
			// committed earlier survives that rejection, which is the same leak
			// staging exists to close, moved one step later. Still before the
			// parent members land, so a parent lands over complete children.
			call.CommitStagedInserts()
			if batch != nil {
				if err := batch.commit(); err != nil {
					p.capErr = err
					return
				}
				copy(inserted, batch.inserted)
			}
			if p.memo != nil {
				for i, newExpr := range yielded {
					if !inserted[i] {
						continue
					}
					switch {
					case t.Phase != PhasePlanning:
						p.memo.Integrate(t.Ref, newExpr)
					case isPhysical(newExpr):
						p.memo.AddExpression(t.Ref, newExpr)
					default:
						p.integratePlanningYield(t.Ref, newExpr)
					}
				}
				if p.capErr != nil {
					return
				}
			}
			if t.Phase == PhasePlanning && len(t.Ref.GetAllPartialMatches()) > matchesBefore {
				progress = true
				p.pushDataAccessTasks(t.Ref, t.Expr)
			}
			for _, in := range inserted {
				progress = progress || in
			}

			for i, newExpr := range yielded {
				if ctx.Err() != nil {
					return
				}
				// A PLANNING yield found in another group merged the two; the
				// group already holds that expression, explored there.
				if !inserted[i] || t.Phase == PhasePlanning && !t.Ref.ContainsExactly(newExpr) {
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
	return progress
}

// TransformImplTask fires a single ImplementationRule on a (group, expression)
// pair. Yields go to final members (ref.InsertFinal).
// Mirrors Java's TransformExpression for ImplementationCascadesRule.
type TransformImplTask struct {
	Phase PlannerPhase
	Ref   *expressions.Reference
	Expr  expressions.RelationalExpression
	Rule  ImplementationRule
	// conditionalIndex is the inner rule a conditional Rule runs next.
	conditionalIndex int
}

// ActiveRule is the rule this task applies: a conditional rule's current inner
// rule, otherwise Rule.
func (t *TransformImplTask) ActiveRule() ImplementationRule {
	if cond, ok := t.Rule.(*conditionalImplementationRule); ok {
		return cond.rules[t.conditionalIndex]
	}
	return t.Rule
}

func (t *TransformImplTask) Run(ctx context.Context, p *Planner) {
	if ctx.Err() != nil || t.Ref == nil || t.Expr == nil || t.Rule == nil {
		return
	}
	cond, conditional := t.Rule.(*conditionalImplementationRule)
	if !conditional {
		t.runRule(ctx, p, t.Rule)
		return
	}
	// Java ConditionalTransformExpression: the next rule runs only when this
	// one made no progress.
	if !t.runRule(ctx, p, cond.rules[t.conditionalIndex]) && t.conditionalIndex+1 < len(cond.rules) && ctx.Err() == nil && p.capErr == nil {
		p.push(&TransformImplTask{Phase: t.Phase, Ref: t.Ref, Expr: t.Expr, Rule: t.Rule, conditionalIndex: t.conditionalIndex + 1})
	}
}

// runRule applies rule to the task's expression and reports progress as Java's
// executeRuleCall does: a new member inserted or a constraint pushed.
func (t *TransformImplTask) runRule(ctx context.Context, p *Planner, rule ImplementationRule) (progress bool) {
	if !t.Ref.ContainsExactly(t.Expr) {
		return
	}
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), t.Expr)
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
			constraintOnly: isPreOrderRule(rule),
		}
		rule.OnMatch(call)
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
		var batch *preparedReferenceBatch
		if len(call.yielded) > 0 {
			intents := make([]referenceMemberIntent, len(call.yielded))
			for i, y := range call.yielded {
				intents[i] = referenceMemberIntent{set: expressions.ReferenceFinalMembers, expression: y}
			}
			prepared, err := prepareReferenceMemberBatch(t.Ref, intents)
			if err != nil {
				p.capErr = err
				return
			}
			batch = prepared
		}
		// After EVERY fallible step — the preflight above AND the batch prepare
		// — and before the parent members land, so a parent still lands over
		// complete children.
		call.CommitStagedInserts()
		if batch != nil {
			if err := batch.commit(); err != nil {
				p.capErr = err
				return
			}
		}
		call.applyPendingConstraints()

		// New final expressions explore and optimize their detached inputs.
		for i, y := range call.yielded {
			if ctx.Err() != nil {
				return
			}
			if !batch.inserted[i] {
				continue
			}
			progress = true
			// InsertFinal only — no re-prune of a stamped group on late final
			// growth, as in Java's executeRuleCall: the new final's inputs are
			// optimized below, and a REWRITING group that reached the stage
			// boundary with a late second final is refused there
			// (RewritingCrossingError), never carried.
			if p.memo != nil {
				p.memo.AddExpression(t.Ref, y)
			}
			if !isExploratoryMember(t.Ref, y) {
				// Rewriting finals need child pruning too; planning logical
				// compensations must not prune correlated inputs standalone.
				if t.Phase == PhaseRewriting || isPhysical(y) {
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
				progress = true
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
				swapBindings := rule.Matcher().BindMatches(matching.NewBindings(), swapped)
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
					rule.OnMatch(call)
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
					var batch *preparedReferenceBatch
					if len(call.yielded) > 0 {
						intents := make([]referenceMemberIntent, len(call.yielded))
						for i, y := range call.yielded {
							intents[i] = referenceMemberIntent{set: expressions.ReferenceFinalMembers, expression: y}
						}
						prepared, err := prepareReferenceMemberBatch(t.Ref, intents)
						if err != nil {
							p.capErr = err
							return
						}
						batch = prepared
					}
					// After EVERY fallible step — the preflight above AND the
					// batch prepare — and before the parent members land.
					call.CommitStagedInserts()
					if batch != nil {
						if err := batch.commit(); err != nil {
							p.capErr = err
							return
						}
					}
					call.applyPendingConstraints()
					for i, y := range call.yielded {
						if !batch.inserted[i] {
							continue
						}
						progress = true
						if ctx.Err() != nil {
							return
						}
						if p.memo != nil {
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
							progress = true
							p.push(&ExploreGroupTask{Phase: t.Phase, Ref: childRef, forceNew: true})
						}
					}
				}
			}
		}
	}
	return progress
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

	costModel, costModelErr := p.costModelForPhase(t.Phase)

	var bestFinal expressions.RelationalExpression
	for _, m := range t.Ref.FinalMembers() {
		if ctx.Err() != nil {
			return
		}
		if bestFinal == nil || costModel(m, bestFinal) {
			bestFinal = m
		}
	}
	if err := costModelErr(); err != nil {
		p.capErr = err
		return
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
	t.Ref.PruneToSet(keep)
	t.Ref.SetWinner(bestFinal)
}

// OptimizeInputsTask pushes OptimizeGroup for each child quantifier.
// Mirrors Java's CascadesPlanner.OptimizeInputs.
type OptimizeInputsTask struct {
	Phase PlannerPhase
	Ref   *expressions.Reference
	Expr  expressions.RelationalExpression

	// Adjacent leaf tasks share this prepass, but retain their identity guards.
	prepassLeaves []expressions.RelationalExpression
}

func (t *OptimizeInputsTask) absorbLeafPrepass(leaf *OptimizeInputsTask) bool {
	if t.Phase != PhasePlanning || leaf.Phase != t.Phase || t.Ref == nil || leaf.Ref == nil ||
		t.Ref.Canonical() != leaf.Ref.Canonical() || t.Expr == nil || leaf.Expr == nil || len(leaf.Expr.GetQuantifiers()) != 0 {
		return false
	}
	if requirements, err := ordinalInputRequirementsOf(leaf.Expr); err != nil || len(requirements) != 0 {
		return false
	}
	t.prepassLeaves = append(t.prepassLeaves, leaf.Expr)
	return true
}

func (t *OptimizeInputsTask) hasWork() bool {
	if t.Expr == nil || len(t.Expr.GetQuantifiers()) != 0 {
		return true
	}
	if _, err := ordinalInputRequirementsOf(t.Expr); err != nil {
		return true
	}
	if t.Phase == PhasePlanning && t.Ref != nil {
		// Even a leaf task must retain a sibling's input prepass or validation error.
		for _, member := range t.Ref.AllMembers() {
			if member == nil || member == t.Expr {
				continue
			}
			if len(member.GetQuantifiers()) != 0 {
				return true
			}
			if _, err := ordinalInputRequirementsOf(member); err != nil {
				return true
			}
		}
	}
	return false
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
	live := t.Ref == nil || t.Ref.ContainsExactly(t.Expr)
	prepass := live
	if !prepass && t.Ref != nil {
		for _, leaf := range t.prepassLeaves {
			if t.Ref.ContainsExactly(leaf) {
				prepass = true
				break
			}
		}
	}
	if !prepass {
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
	if !live {
		return
	}
	requirements, err := ordinalInputRequirementsOf(t.Expr)
	if err != nil {
		p.capErr = err
		return
	}
	dependentFloor := len(p.stack)
	// Java OptimizeInputs: the pruned-inputs rules are pushed beneath the
	// input groups' OptimizeGroup tasks, so they see every input pruned.
	if t.Ref != nil {
		_, implIdx := p.ruleIndexesForPhase(t.Phase)
		rules := implIdx.rulesFor(t.Expr)
		for i := len(rules) - 1; i >= 0; i-- {
			if isPrunedInputsRule(rules[i]) {
				p.push(&TransformImplTask{Phase: t.Phase, Ref: t.Ref, Expr: t.Expr, Rule: rules[i]})
			}
		}
	}
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
		if t.Phase == PhasePlanning {
			completed, err := p.completePinnedFinal(childRef)
			if err != nil {
				p.capErr = err
				return
			}
			if completed {
				continue
			}
		}
		childRefs = append(childRefs, childRef)
	}
	if t.Phase == PhasePlanning {
		completed, err := p.completeSingletonInputs(ctx, childRefs)
		if err != nil {
			p.capErr = err
			return
		}
		if completed {
			return
		}
	}
	for _, childRef := range childRefs {
		p.push(&OptimizeGroupTask{Phase: t.Phase, Ref: childRef})
	}
	p.scheduleExploreGroupsBeforeBatch(t.Phase, childRefs, dependentFloor)
}

// A settled singleton has no cost choice. Complete only whole input batches:
// another child's scheduled work could otherwise change an omitted sibling.
func (p *Planner) settledSingletonInputs(refs []*expressions.Reference) ([]*expressions.Reference, bool) {
	if len(refs) == 0 {
		return nil, false
	}
	members := make(map[*expressions.Reference]bool, len(refs))
	ordered := make([]*expressions.Reference, 0, len(refs))
	for _, ref := range refs {
		ref = ref.Canonical()
		constraints := ref.ConstraintsMap()
		finals := ref.FinalMembers()
		if ref.Stage() != expressions.StagePlanned || len(ref.Members()) != 0 || len(finals) != 1 ||
			!isPhysical(finals[0]) || !constraints.IsExplored() || constraints.IsExploring() {
			return nil, false
		}
		if _, duplicate := members[ref]; !duplicate {
			ordered = append(ordered, ref)
			members[ref] = true
		}
	}
	if p.hasPendingInputWork(members) {
		return nil, false
	}
	return ordered, true
}

func (p *Planner) hasPendingInputWork(refs map[*expressions.Reference]bool) bool {
	for _, task := range p.stack {
		var ref *expressions.Reference
		switch task := task.(type) {
		case *ExploreGroupTask:
			ref = task.Ref
		case *ExploreExprTask:
			ref = task.Ref
		case *OptimizeGroupTask:
			ref = task.Ref
		case *OptimizeInputsTask:
			ref = task.Ref
		case *TransformExprTask:
			ref = task.Ref
		case *TransformImplTask:
			ref = task.Ref
		case *ConsumeMatchPartitionTask:
			ref = task.Ref
		case *TransformMatchPartitionTask:
			ref = task.Ref
		default:
			return true
		}
		if _, pending := refs[ref.Canonical()]; pending {
			return true
		}
	}
	return false
}

func (p *Planner) completeSingletonInputs(ctx context.Context, refs []*expressions.Reference) (bool, error) {
	ordered, settled := p.settledSingletonInputs(refs)
	if !settled {
		return false, nil
	}
	// Preserve the reverse input order of the LIFO optimizer batch.
	for i := len(ordered) - 1; i >= 0; i-- {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		ref := ordered[i]
		member := ref.FinalMembers()[0]
		computeRefPlanProperties(ref)
		if requirements, ok := Get(p.constraintMap, ref, OrdinalLayoutConstraintKey); ok {
			for _, requirement := range requirements {
				compatible, err := memberSatisfiesOrdinalRequirement(member, requirement)
				if err != nil {
					return false, fmt.Errorf("ordinal layout winner for child group: %w", err)
				}
				if !compatible {
					return false, fmt.Errorf("ordinal layout winner for child group: no compatible final member remains")
				}
			}
		}
		ref.SetWinner(member)
	}
	return true, nil
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

// isPrunedInputsRule reports Java's CascadesRule.onlyOnPrunedInputs: such a
// rule never fires while an expression is explored, only from OptimizeInputs
// once every input group has been pruned to its cheapest final member.
func isPrunedInputsRule(rule ImplementationRule) bool {
	type prunedInputs interface {
		OnlyOnPrunedInputs() bool
	}
	if pi, ok := rule.(prunedInputs); ok {
		return pi.OnlyOnPrunedInputs()
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
