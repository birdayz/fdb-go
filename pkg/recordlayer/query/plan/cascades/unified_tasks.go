package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
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
	if t.Phase.HasNextPhase() {
		p.push(&InitiatePlannerPhaseTask{Phase: t.Phase.NextPhase(), RootRef: t.RootRef})
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
		t.Ref.AdvancePlannerStage(targetStage)
	}

	if !t.Ref.NeedsExploration() {
		t.Ref.CommitExploration()
		return
	}

	// Re-push this task to check convergence after exploration completes.
	p.push(t)

	// Final members get exploreExpressionAndOptimizeInputs.
	for _, expr := range t.Ref.FinalMembers() {
		p.push(&OptimizeInputsTask{Phase: t.Phase, Ref: t.Ref, Expr: expr})
		p.push(&ExploreExprTask{Phase: t.Phase, Ref: t.Ref, Expr: expr})
	}
	// Exploratory members get just exploreExpression.
	for _, expr := range t.Ref.Members() {
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

	exprRules, implRules := p.rulesForPhase(t.Phase)

	// 1. Push match-partition rules (fire LAST — deepest on LIFO).
	// Data access generation from PartialMatches.
	if t.Phase == PhasePlanning {
		p.pushDataAccessTasks(t.Ref, t.Expr)
	}

	// 2. Push non-preorder implementation rules.
	for i := len(implRules) - 1; i >= 0; i-- {
		rule := implRules[i]
		if isPreOrderRule(rule) {
			continue
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
	bindings := t.Rule.Matcher().BindMatches(matching.NewBindings(), t.Expr)
	for _, b := range bindings {
		call := &ExpressionRuleCall{
			Bindings:  b,
			Reference: t.Ref,
			Context:   p.ctx,
			memo:      p.memo,
		}
		t.Rule.OnMatch(call)

		// Handle new exploratory yields: push ExploreExprTask for each.
		for _, newExpr := range call.Yielded() {
			p.push(&ExploreExprTask{Phase: t.Phase, Ref: t.Ref, Expr: newExpr})
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
	bindings := t.Rule.Matcher().BindMatches(matching.NewBindings(), t.Expr)
	for _, b := range bindings {
		call := &ImplementationRuleCall{
			Bindings:    b,
			Reference:   t.Ref,
			Context:     p.ctx,
			Constraints: p.constraintMap,
			memo:        p.memo,
		}
		t.Rule.OnMatch(call)

		// Handle yields: insert into FinalMembers and push explore+optimize
		// for genuinely new expressions. Skip re-exploration for
		// FinalizeExpressionsRule yields (they're already-explored
		// exploratory members promoted to final).
		for _, y := range call.yielded {
			t.Ref.InsertFinal(y)
			if !isAlreadyExploratoryMember(t.Ref, y) {
				p.push(&OptimizeInputsTask{Phase: t.Phase, Ref: t.Ref, Expr: y})
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
	if sel, ok := t.Expr.(*expressions.SelectExpression); ok && sel.ChildrenAsSet() {
		qs := sel.GetQuantifiers()
		if len(qs) >= 2 && sel.GetJoinType() != expressions.JoinLeftOuter &&
			qs[0].Kind() == expressions.QuantifierForEach &&
			qs[1].Kind() == expressions.QuantifierForEach {
			swapped := sel.WithSwappedQuantifiers()
			t2 := &TransformImplTask{Phase: t.Phase, Ref: t.Ref, Expr: swapped, Rule: t.Rule}
			t2.Run(p)
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
	costModel := p.costModelForPhase(t.Phase)

	var bestFinal expressions.RelationalExpression
	for _, m := range t.Ref.FinalMembers() {
		if bestFinal == nil || costModel(m, bestFinal) {
			bestFinal = m
		}
	}

	if bestFinal != nil {
		t.Ref.PruneWith(bestFinal)
	} else {
		t.Ref.ClearFinalMembers()
	}

	// Also stamp ordering-specific winners for sort elimination.
	if t.Phase == PhasePlanning {
		stampOrderingWinners(t.Ref, costModel)
	}
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
	for _, q := range t.Expr.GetQuantifiers() {
		childRef := q.GetRangesOver()
		if childRef == nil {
			continue
		}
		p.push(&OptimizeGroupTask{Phase: t.Phase, Ref: childRef})
		p.push(&ExploreGroupTask{Phase: t.Phase, Ref: childRef})
	}
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
