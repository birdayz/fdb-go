package cascades

import (
	"fmt"
	"slices"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
)

// PLANNING-phase group merging (Graefe 1995 §2, §3.5). Every logical
// expression a PLANNING rule yields is looked up by operator and input groups;
// when it already lives in another group, the two groups are one equivalence
// class and merge. As in the paper, the merged group's goals are the union of
// both groups' goals: a goal new to one side's members is explored for them,
// and nothing either side already explored under the merged goals is redone.
//
// Java never merges groups. Wire compatibility is untouched: merging changes
// which groups the search visits, never what a plan reads or writes.

// integratePlanningYield indexes a logical PLANNING yield and merges its group
// with any group already holding an exact replica, then re-checks the merged
// group's parents, which may now be replicas of one another.
func (p *Planner) integratePlanningYield(ref *expressions.Reference, expr expressions.RelationalExpression) {
	if p.memo == nil {
		return
	}
	p.memo.AddExpression(ref, expr)
	if p.withoutPlanningMerges {
		return
	}
	pending := []parentEdge{{parent: ref, expr: expr}}
	for len(pending) > 0 && p.capErr == nil {
		edge := pending[0]
		pending = pending[1:]
		group := edge.parent.Canonical()
		if !isExploratoryMember(group, edge.expr) {
			continue
		}
		mergeable := func(other *expressions.Reference) bool { return p.memo.mergeablePlanning(group, other) }
		if other := p.memo.findExactReplicaRef(edge.expr, mergeable); other != nil {
			pending = append(pending, p.mergePlanningGroups(group, other)...)
			continue
		}
		p.removeReplicaWithinGroup(group, edge.expr)
	}
}

// mergePlanningGroups folds one group into the other and returns the loser's
// parent edges, whose expressions now range over the survivor.
func (p *Planner) mergePlanningGroups(a, b *expressions.Reference) []parentEdge {
	survivor, loser := planningMergeOrder(a.Canonical(), b.Canonical())
	exploratory, finals := loser.Members(), loser.FinalMembers()
	intents := make([]referenceMemberIntent, 0, len(exploratory)+len(finals))
	for _, member := range exploratory {
		intents = append(intents, referenceMemberIntent{set: expressions.ReferenceExploratoryMembers, expression: member})
	}
	for _, member := range finals {
		intents = append(intents, referenceMemberIntent{set: expressions.ReferenceFinalMembers, expression: member})
	}
	// Checked admission: two groups holding one expression must agree on
	// their result type, or the replica test is wrong.
	batch, err := prepareReferenceMemberBatch(survivor, intents)
	if err != nil {
		p.capErr = fmt.Errorf("merging equivalent planning groups: %w", err)
		return nil
	}
	survivorStarted := explorationProgress(survivor) > 0
	loserState := loser.ConstraintsMap()
	loserStarted := explorationProgress(loser) > 0
	parents := slices.Clone(p.memo.childToParents[loser])
	if err := batch.commit(); err != nil {
		p.capErr = fmt.Errorf("merging equivalent planning groups: %w", err)
		return nil
	}
	newMatches := survivor.AbsorbPlanningState(loser)
	p.memo.repointIndices(loser, survivor)
	p.memo.invalidateCorrelatedUp(survivor)
	p.memo.mergeCount++

	tick := survivor.ConstraintsMap().CurrentTick()
	loserConstraints := p.constraintMap.rehome(loser, survivor)
	p.foldConsumption(loser, survivor)
	if survivor.GetPlanProperties() != nil {
		computeRefPlanProperties(survivor)
	}

	if survivorStarted {
		p.supersedeExploreGroupTasks(loser)
	}
	// A goal the loser's parents pushed is new to every survivor member.
	if survivorStarted && survivor.ConstraintsMap().CurrentTick() != tick {
		p.push(&ExploreGroupTask{Phase: PhasePlanning, Ref: survivor, forceNew: true})
	}
	// The folded members are new to the survivor; they need exploring unless
	// the loser already explored them under every survivor goal.
	if survivorStarted && !(loserStarted && p.exploredUnderGoalsOf(survivor, loserState, loserConstraints)) {
		p.exploreFoldedMembers(survivor, intents, batch.inserted)
	}
	if newMatches > 0 {
		p.pushDataAccessTasks(survivor, nil)
	}
	return parents
}

// planningMergeOrder keeps the group further into its exploration, so a merge
// never repeats exploration the other group already did; ties keep the older.
func planningMergeOrder(a, b *expressions.Reference) (survivor, loser *expressions.Reference) {
	if progressA, progressB := explorationProgress(a), explorationProgress(b); progressA != progressB {
		if progressA > progressB {
			return a, b
		}
		return b, a
	}
	if b.ID() < a.ID() {
		return b, a
	}
	return a, b
}

func explorationProgress(ref *expressions.Reference) int {
	state := ref.ConstraintsMap()
	switch {
	case !state.HasNeverBeenExplored():
		return 2
	case state.IsExploring():
		return 1
	}
	return 0
}

// exploredUnderGoalsOf reports whether the loser's exploration, as its
// constraint map records it, covered every constraint the survivor holds.
func (p *Planner) exploredUnderGoalsOf(
	survivor *expressions.Reference,
	loserState *expressions.ConstraintsMap,
	loserConstraints []constraintValue,
) bool {
	if !loserState.IsExploredForAttributes(nil) {
		return false
	}
	explored := make(map[any]any, len(loserConstraints))
	for _, constraint := range loserConstraints {
		explored[constraint.key] = constraint.value
	}
	for _, constraint := range p.constraintMap.constraintsOf(survivor) {
		value, ok := explored[constraint.key]
		if !ok || !loserState.IsExploredForAttributes([]any{constraint.key}) {
			return false
		}
		if _, grows := combineForKey(constraint.key)(value, constraint.value); grows {
			return false
		}
	}
	return true
}

// exploreFoldedMembers schedules the members the loser brought into an
// explored survivor, exactly as an insert into the survivor would.
func (p *Planner) exploreFoldedMembers(survivor *expressions.Reference, intents []referenceMemberIntent, inserted []bool) {
	for i, intent := range intents {
		if !inserted[i] || !isPhysical(intent.expression) && intent.set == expressions.ReferenceFinalMembers {
			continue
		}
		if isPhysical(intent.expression) {
			p.push(&OptimizeInputsTask{Phase: PhasePlanning, Ref: survivor, Expr: intent.expression})
		}
		p.push(&ExploreExprTask{Phase: PhasePlanning, Ref: survivor, Expr: intent.expression})
	}
}

// supersedeExploreGroupTasks retires the group tasks queued for the loser: the
// merge scheduled the exploration the fold needs, and the survivor's rounds
// belong to the survivor. Against a survivor not yet explored they stay, as
// its first visit ahead of the loser's parents.
func (p *Planner) supersedeExploreGroupTasks(loser *expressions.Reference) {
	for _, task := range p.stack {
		group, ok := task.(*ExploreGroupTask)
		if !ok || !group.pending || group.pendingKey.ref != loser {
			continue
		}
		if remaining := p.pendingExploreGroups[group.pendingKey] - 1; remaining > 0 {
			p.pendingExploreGroups[group.pendingKey] = remaining
		} else {
			delete(p.pendingExploreGroups, group.pendingKey)
		}
		group.pending = false
		group.superseded = true
	}
}

func (p *Planner) foldConsumption(loser, survivor *expressions.Reference) {
	if records, ok := p.dataAccessConsumed[loser]; ok {
		p.dataAccessConsumed[survivor] = append(p.dataAccessConsumed[survivor], records...)
		delete(p.dataAccessConsumed, loser)
	}
	if records, ok := p.intersectionConsumed[loser]; ok {
		p.intersectionConsumed[survivor] = append(p.intersectionConsumed[survivor], records...)
		delete(p.intersectionConsumed, loser)
	}
}

// removeReplicaWithinGroup removes the later of two exploratory members a
// merge made exact replicas of each other. The earlier one stays: its
// exploration has run or is pending.
func (p *Planner) removeReplicaWithinGroup(group *expressions.Reference, expr expressions.RelationalExpression) {
	members := group.Members()
	at := slices.Index(members, expr)
	hash := group.MemberHash(expr)
	for i, member := range members {
		if i == at || group.MemberHash(member) != hash || !expressions.ExactReplica(member, expr) {
			continue
		}
		later := expr
		if i > at {
			later = member
		}
		if group.RemoveExploratoryMember(later) {
			p.memo.replicasRemoved++
		}
		return
	}
}
