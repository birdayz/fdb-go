package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestPlannerCoalescesOnlyPendingExplorationTasks(t *testing.T) {
	t.Parallel()

	rowType := values.NewRecordType("T", false, nil)
	first, err := expressions.NewFullUnorderedScanExpression([]string{"T"}, rowType)
	if err != nil {
		t.Fatalf("construct first scan: %v", err)
	}
	second, err := expressions.NewFullUnorderedScanExpression([]string{"U"}, rowType)
	if err != nil {
		t.Fatalf("construct second scan: %v", err)
	}
	ref := expressions.InitialOf(first)
	p := NewPlanner(nil, EmptyPlanContext())

	p.push(&ExploreGroupTask{Phase: PhaseRewriting, Ref: ref})
	p.push(&ExploreGroupTask{Phase: PhaseRewriting, Ref: ref})
	if got := len(p.stack); got != 1 {
		t.Fatalf("duplicate pending group task grew the stack to %d", got)
	}

	// Phase is part of the work identity: planning must not hide behind a
	// pending rewriting task for the same memo group.
	p.push(&ExploreGroupTask{Phase: PhasePlanning, Ref: ref})
	if got := len(p.stack); got != 2 {
		t.Fatalf("distinct-phase group task was coalesced: stack=%d", got)
	}
	if got := p.pop(); got.(*ExploreGroupTask).Phase != PhasePlanning {
		t.Fatalf("planner lost LIFO order: popped %#v", got)
	}

	// A task stops being pending when popped. Scheduling the same work again
	// is therefore legal: it may observe members or constraints produced by
	// the task that just ran.
	p.push(&ExploreGroupTask{Phase: PhasePlanning, Ref: ref})
	if got := len(p.stack); got != 2 {
		t.Fatalf("popped group task remained permanently suppressed: stack=%d", got)
	}
	for len(p.stack) > 0 {
		p.pop()
	}

	p.push(&OptimizeGroupTask{Phase: PhaseRewriting, Ref: ref})
	p.push(&OptimizeGroupTask{Phase: PhaseRewriting, Ref: ref})
	p.push(&OptimizeGroupTask{Phase: PhasePlanning, Ref: ref})
	if got := len(p.stack); got != 2 {
		t.Fatalf("pending optimizer identity lost phase/group semantics: stack=%d", got)
	}
	p.pop()
	p.push(&OptimizeGroupTask{Phase: PhasePlanning, Ref: ref})
	if got := len(p.stack); got != 2 {
		t.Fatalf("popped optimizer remained permanently suppressed: stack=%d", got)
	}
	for len(p.stack) > 0 {
		p.pop()
	}

	p.push(&ExploreExprTask{Phase: PhaseRewriting, Ref: ref, Expr: first})
	p.push(&ExploreExprTask{Phase: PhaseRewriting, Ref: ref, Expr: first})
	if got := len(p.stack); got != 1 {
		t.Fatalf("duplicate pending expression task grew the stack to %d", got)
	}

	p.push(&ExploreExprTask{Phase: PhaseRewriting, Ref: ref, Expr: second})
	p.push(&ExploreExprTask{Phase: PhasePlanning, Ref: ref, Expr: first})
	if got := len(p.stack); got != 3 {
		t.Fatalf("expression or phase discriminator was lost: stack=%d", got)
	}

	for len(p.stack) > 0 {
		p.pop()
	}

	// Memo integration can forward a group while its exploration task waits
	// on the stack. The survivor must still coalesce with that pending work,
	// and popping the loser task must release the original pending key.
	loser := expressions.InitialOf(first)
	survivor := expressions.InitialOf(first)
	p.push(&ExploreGroupTask{Phase: PhaseRewriting, Ref: loser})
	survivor.Absorb(loser)
	p.push(&ExploreGroupTask{Phase: PhaseRewriting, Ref: survivor})
	if got := len(p.stack); got != 1 {
		t.Fatalf("forwarded group gained duplicate pending work: stack=%d", got)
	}
	p.pop()
	p.push(&ExploreGroupTask{Phase: PhaseRewriting, Ref: survivor})
	if got := len(p.stack); got != 1 {
		t.Fatalf("forwarded task left its captured pending key behind: stack=%d", got)
	}
	p.pop()

	optLoser := expressions.InitialOf(first)
	optSurvivor := expressions.InitialOf(first)
	p.push(&OptimizeGroupTask{Phase: PhasePlanning, Ref: optLoser})
	optSurvivor.Absorb(optLoser)
	p.push(&OptimizeGroupTask{Phase: PhasePlanning, Ref: optSurvivor})
	if got := len(p.stack); got != 1 {
		t.Fatalf("forwarded group gained duplicate pending optimization: stack=%d", got)
	}
	p.pop()
	p.push(&OptimizeGroupTask{Phase: PhasePlanning, Ref: optSurvivor})
	if got := len(p.stack); got != 1 {
		t.Fatalf("forwarded optimizer left its captured pending key behind: stack=%d", got)
	}
	p.pop()

	exprLoser := expressions.InitialOf(first)
	exprSurvivor := expressions.InitialOf(first)
	p.push(&ExploreExprTask{Phase: PhaseRewriting, Ref: exprLoser, Expr: first})
	exprSurvivor.Absorb(exprLoser)
	p.push(&ExploreExprTask{Phase: PhaseRewriting, Ref: exprSurvivor, Expr: first})
	if got := len(p.stack); got != 1 {
		t.Fatalf("forwarded expression gained duplicate pending work: stack=%d", got)
	}
	p.pop()
	p.push(&ExploreExprTask{Phase: PhaseRewriting, Ref: exprSurvivor, Expr: first})
	if got := len(p.stack); got != 1 {
		t.Fatalf("forwarded expression task left its captured key behind: stack=%d", got)
	}
	p.pop()
	if len(p.pendingExploreGroups) != 0 || len(p.pendingExploreExprs) != 0 || len(p.pendingOptimizeGroups) != 0 {
		t.Fatalf("drained stack retained pending work: groups=%d expressions=%d optimizers=%d",
			len(p.pendingExploreGroups), len(p.pendingExploreExprs), len(p.pendingOptimizeGroups))
	}
}

func TestPlannerDoesNotCoalescePostTransitionContinuationWithForwardedPendingTask(t *testing.T) {
	t.Parallel()

	rowType := values.NewRecordType("T", false, nil)
	scan, err := expressions.NewFullUnorderedScanExpression([]string{"T"}, rowType)
	if err != nil {
		t.Fatalf("construct scan: %v", err)
	}
	loser := expressions.InitialOf(scan)
	survivor := expressions.InitialOf(scan)
	p := NewPlanner(nil, EmptyPlanContext())

	// These are distinct groups when queued, so both tasks are pending. Memo
	// integration can forward them while they wait on the LIFO stack.
	p.push(&ExploreGroupTask{Phase: PhasePlanning, Ref: loser})
	p.push(&ExploreGroupTask{Phase: PhasePlanning, Ref: survivor})
	if got := len(p.stack); got != 2 {
		t.Fatalf("distinct groups did not queue distinct tasks: stack=%d", got)
	}
	survivor.Absorb(loser)

	// The top task starts the canonical group's planning-stage round. Its
	// continuation must sit above the stale pre-transition task; otherwise a
	// parent optimizer between them can run before this round converges.
	p.pop()
	survivor.AdvanceStagePreservingMembers(expressions.StagePlanned)
	p.push(&ExploreGroupTask{Phase: PhasePlanning, Ref: survivor})
	if got := len(p.stack); got != 2 {
		t.Fatalf("post-transition continuation coalesced with buried pre-transition task: stack=%d", got)
	}
}

func TestPlannerMovesPendingExploreGroupAboveLIFOBarrier(t *testing.T) {
	t.Parallel()

	rowType := values.NewRecordType("T", false, nil)
	scan, err := expressions.NewFullUnorderedScanExpression([]string{"T"}, rowType)
	if err != nil {
		t.Fatalf("construct scan: %v", err)
	}
	child := expressions.InitialOf(scan)
	parent := expressions.InitialOf(scan)
	p := NewPlanner(nil, EmptyPlanContext())

	// The child exploration is already pending below a newly pushed parent
	// optimizer. Reusing it is safe only if the dependent batch moves behind it;
	// otherwise the parent observes an unimplemented child (the nested-UNION
	// fuzz regression).
	p.push(&ExploreGroupTask{Phase: PhasePlanning, Ref: child})
	dependentFloor := len(p.stack)
	p.push(&OptimizeGroupTask{Phase: PhasePlanning, Ref: parent})
	p.scheduleExploreGroupsBeforeBatch(PhasePlanning, []*expressions.Reference{child}, dependentFloor)
	if got := len(p.stack); got != 2 {
		t.Fatalf("dependency scheduling duplicated work: stack=%d, want 2", got)
	}
	if top, ok := p.stack[len(p.stack)-1].(*ExploreGroupTask); !ok || top.Ref.Canonical() != child.Canonical() {
		t.Fatalf("pending child exploration is not next to run: top=%T", p.stack[len(p.stack)-1])
	}
}
