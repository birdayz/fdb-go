package cascades

import (
	"context"
	"errors"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// planningMergeFixture is a PLANNING-phase planner over filters of one scan,
// each group explored or not as a test needs.
type planningMergeFixture struct {
	planner *Planner
	scan    *expressions.Reference
}

func newPlanningMergeFixture(t testing.TB) *planningMergeFixture {
	t.Helper()
	scan := expressions.ExploratoryOfAtStage(memoTestScan(t, "T"), expressions.StagePlanned)
	planner := NewPlanner(nil, EmptyPlanContext())
	planner.memo = NewMemo(scan)
	planner.memo.MarkPlanningActive()
	planner.activePhase = PhasePlanning
	planner.constraintMap = NewConstraintMap()
	planner.dataAccessConsumed = make(map[*expressions.Reference][]matchConsumption)
	planner.intersectionConsumed = make(map[*expressions.Reference][]intersectionConsumption)
	return &planningMergeFixture{planner: planner, scan: scan}
}

// planningFilter is Filter(value) over child through a quantifier named alias;
// two calls with equal arguments are exact replicas.
func planningFilter(t testing.TB, value predicates.TriBool, alias string, child *expressions.Reference) expressions.RelationalExpression {
	t.Helper()
	return memoTestFilter(t, []predicates.QueryPredicate{predicates.NewConstantPredicate(value)},
		expressions.NamedForEachQuantifier(values.NamedCorrelationIdentifier(alias), child))
}

func (f *planningMergeFixture) group(members ...expressions.RelationalExpression) *expressions.Reference {
	ref := expressions.ExploratoryOfAtStage(members[0], expressions.StagePlanned)
	for _, member := range members[1:] {
		ref.Insert(member)
	}
	f.planner.memo.RegisterReference(ref)
	return ref
}

// yield inserts expr into ref and integrates it, as a rule yield does.
func (f *planningMergeFixture) yield(t testing.TB, ref *expressions.Reference, expr expressions.RelationalExpression) {
	t.Helper()
	if !ref.Insert(expr) {
		t.Fatal("fixture yield was a duplicate within its own group")
	}
	f.planner.integratePlanningYield(ref, expr)
	if f.planner.capErr != nil {
		t.Fatal(f.planner.capErr)
	}
}

func explored(refs ...*expressions.Reference) {
	for _, ref := range refs {
		ref.StartExploration()
		ref.CommitExploration()
	}
}

type queuedWork struct{ groupRounds, memberExplorations int }

func (f *planningMergeFixture) queued(ref *expressions.Reference) queuedWork {
	var work queuedWork
	for _, task := range f.planner.stack {
		switch task := task.(type) {
		case *ExploreGroupTask:
			if task.Ref == ref && !task.superseded {
				work.groupRounds++
			}
		case *ExploreExprTask:
			if task.Ref == ref {
				work.memberExplorations++
			}
		}
	}
	return work
}

func TestPlanningMergeFoldsGroupIntoSurvivor(t *testing.T) {
	t.Parallel()
	f := newPlanningMergeFixture(t)
	survivor := f.group(planningFilter(t, predicates.TriTrue, "Q", f.scan))
	loser := f.group(planningFilter(t, predicates.TriFalse, "Q", f.scan))
	explored(survivor, loser)
	// Pushed after the loser's round: the loser never explored under it.
	ordering := []*properties.RequestedOrdering{properties.PreserveOrdering()}
	Set(f.planner.constraintMap, loser, RequestedOrderingConstraintKey, ordering)
	f.planner.dataAccessConsumed[loser] = []matchConsumption{{matches: map[PartialMatch]struct{}{}}}
	folded := loser.Members()[0]

	replica := planningFilter(t, predicates.TriTrue, "Q", f.scan)
	f.yield(t, loser, replica)

	if f.planner.memo.MergeCount() != 1 || loser.Canonical() != survivor || survivor.Canonical() != survivor {
		t.Fatalf("merges=%d: the loser must forward to the older survivor", f.planner.memo.MergeCount())
	}
	if !survivor.ContainsExactly(folded) || survivor.ContainsExactly(replica) || len(survivor.Members()) != 2 {
		t.Fatalf("survivor must hold both groups' members once: %v", survivor.Members())
	}
	if got, ok := Get(f.planner.constraintMap, survivor, RequestedOrderingConstraintKey); !ok || len(got) != 1 {
		t.Fatalf("the loser's parents' ordering must constrain the survivor: %v %v", got, ok)
	}
	if left := f.planner.constraintMap.constraintsOf(loser); len(left) != 0 {
		t.Fatalf("constraints left keyed by the loser: %v", left)
	}
	if len(f.planner.dataAccessConsumed[survivor]) != 1 || f.planner.dataAccessConsumed[loser] != nil {
		t.Fatal("the loser's consumed match partitions must move to the survivor")
	}
	// The ordering is new to the survivor's members, and the folded member
	// never explored under it.
	if got := f.queued(survivor); got != (queuedWork{groupRounds: 1, memberExplorations: 1}) {
		t.Fatalf("queued %+v, want one survivor round and the folded member's exploration", got)
	}
}

func TestPlanningMergeRedoesNothingBothGroupsExplored(t *testing.T) {
	t.Parallel()
	f := newPlanningMergeFixture(t)
	survivor := f.group(planningFilter(t, predicates.TriTrue, "Q", f.scan))
	loser := f.group(planningFilter(t, predicates.TriFalse, "Q", f.scan))
	ordering := []*properties.RequestedOrdering{properties.PreserveOrdering()}
	Set(f.planner.constraintMap, survivor, RequestedOrderingConstraintKey, ordering)
	Set(f.planner.constraintMap, loser, RequestedOrderingConstraintKey, ordering)
	explored(survivor, loser)

	f.yield(t, loser, planningFilter(t, predicates.TriTrue, "Q", f.scan))

	if f.planner.memo.MergeCount() != 1 {
		t.Fatal("equivalent groups did not merge")
	}
	if len(f.planner.stack) != 0 {
		t.Fatalf("both groups explored under the merged goals; queued %d tasks", len(f.planner.stack))
	}
}

func TestPlanningMergeExploresNeverExploredLoserOnce(t *testing.T) {
	t.Parallel()
	f := newPlanningMergeFixture(t)
	survivor := f.group(planningFilter(t, predicates.TriTrue, "Q", f.scan))
	loser := f.group(planningFilter(t, predicates.TriFalse, "Q", f.scan))
	explored(survivor)
	visit := &ExploreGroupTask{Phase: PhasePlanning, Ref: loser}
	f.planner.push(visit)
	folded := loser.Members()[0]

	f.yield(t, loser, planningFilter(t, predicates.TriTrue, "Q", f.scan))

	if !visit.superseded || len(f.planner.pendingExploreGroups) != 0 {
		t.Fatal("the loser's queued visit must be retired; the merge schedules its members")
	}
	if got := f.queued(survivor); got != (queuedWork{memberExplorations: 1}) {
		t.Fatalf("queued %+v, want exactly the folded member's exploration", got)
	}
	var explores *ExploreExprTask
	for _, task := range f.planner.stack {
		if task, ok := task.(*ExploreExprTask); ok {
			explores = task
		}
	}
	if explores.Expr != folded || explores.ReExplore {
		t.Fatal("the folded member must be explored as a new member")
	}
}

func TestPlanningMergeKeepsVisitOfUnexploredSurvivor(t *testing.T) {
	t.Parallel()
	f := newPlanningMergeFixture(t)
	survivor := f.group(planningFilter(t, predicates.TriTrue, "Q", f.scan))
	loser := f.group(planningFilter(t, predicates.TriFalse, "Q", f.scan))
	visit := &ExploreGroupTask{Phase: PhasePlanning, Ref: loser}
	f.planner.push(visit)

	f.yield(t, loser, planningFilter(t, predicates.TriTrue, "Q", f.scan))

	if loser.Canonical() != survivor || visit.superseded || len(f.planner.stack) != 1 {
		t.Fatal("the queued visit is the merged group's first round and must stay")
	}
}

func TestPlanningMergeGuards(t *testing.T) {
	t.Parallel()
	t.Run("ancestor", func(t *testing.T) {
		t.Parallel()
		f := newPlanningMergeFixture(t)
		descendant := f.group(planningFilter(t, predicates.TriTrue, "Q", f.scan))
		ancestor := f.group(planningFilter(t, predicates.TriFalse, "P", descendant))
		f.yield(t, ancestor, planningFilter(t, predicates.TriTrue, "Q", f.scan))
		if f.planner.memo.MergeCount() != 0 || ancestor.Canonical() != ancestor {
			t.Fatal("a group merged with its own descendant ranges over itself")
		}
	})
	t.Run("not yet planned", func(t *testing.T) {
		t.Parallel()
		f := newPlanningMergeFixture(t)
		canonical := expressions.ExploratoryOfAtStage(planningFilter(t, predicates.TriTrue, "Q", f.scan), expressions.StageCanonical)
		f.planner.memo.RegisterReference(canonical)
		loser := f.group(planningFilter(t, predicates.TriFalse, "Q", f.scan))
		f.yield(t, loser, planningFilter(t, predicates.TriTrue, "Q", f.scan))
		if f.planner.memo.MergeCount() != 0 || canonical.Canonical() != canonical {
			t.Fatal("a group still at the REWRITING stage merged with a PLANNING group")
		}
	})
	t.Run("an ineligible replica does not hide an eligible one", func(t *testing.T) {
		t.Parallel()
		f := newPlanningMergeFixture(t)
		canonical := expressions.ExploratoryOfAtStage(planningFilter(t, predicates.TriTrue, "Q", f.scan), expressions.StageCanonical)
		f.planner.memo.RegisterReference(canonical)
		planned := f.group(planningFilter(t, predicates.TriTrue, "Q", f.scan))
		loser := f.group(planningFilter(t, predicates.TriFalse, "Q", f.scan))
		f.yield(t, loser, planningFilter(t, predicates.TriTrue, "Q", f.scan))
		if loser.Canonical() != planned || canonical.Canonical() != canonical {
			t.Fatal("the yield must merge with the PLANNING group holding it")
		}
	})
	t.Run("pinned", func(t *testing.T) {
		t.Parallel()
		f := newPlanningMergeFixture(t)
		pinned := expressions.PinnedFinalOf(physicalYieldFixture(t, "T", values.NotNullLong))
		pinned.Insert(planningFilter(t, predicates.TriTrue, "Q", f.scan))
		f.planner.memo.RegisterReference(pinned)
		loser := f.group(planningFilter(t, predicates.TriFalse, "Q", f.scan))
		if f.planner.memo.mergeablePlanning(loser, pinned) {
			t.Fatal("a pinned final is an exact selection, not a group to merge")
		}
		f.yield(t, loser, planningFilter(t, predicates.TriTrue, "Q", f.scan))
		if f.planner.memo.MergeCount() != 0 {
			t.Fatal("merged into a pinned final")
		}
	})
}

func TestPlanningMergeRechecksParents(t *testing.T) {
	t.Parallel()
	t.Run("parents in two groups merge", func(t *testing.T) {
		t.Parallel()
		f := newPlanningMergeFixture(t)
		survivor := f.group(planningFilter(t, predicates.TriTrue, "Q", f.scan))
		loser := f.group(planningFilter(t, predicates.TriFalse, "Q", f.scan))
		overSurvivor := f.group(planningFilter(t, predicates.TriTrue, "P", survivor))
		overLoser := f.group(planningFilter(t, predicates.TriTrue, "P", loser))
		f.yield(t, loser, planningFilter(t, predicates.TriTrue, "Q", f.scan))
		if f.planner.memo.MergeCount() != 2 || overLoser.Canonical() != overSurvivor {
			t.Fatalf("merges=%d: parents over one merged group are one group", f.planner.memo.MergeCount())
		}
	})
	t.Run("parents in one group lose the replica", func(t *testing.T) {
		t.Parallel()
		f := newPlanningMergeFixture(t)
		survivor := f.group(planningFilter(t, predicates.TriTrue, "Q", f.scan))
		loser := f.group(planningFilter(t, predicates.TriFalse, "Q", f.scan))
		earlier := planningFilter(t, predicates.TriTrue, "P", survivor)
		parent := f.group(earlier, planningFilter(t, predicates.TriTrue, "P", loser))
		f.yield(t, loser, planningFilter(t, predicates.TriTrue, "Q", f.scan))
		if f.planner.memo.ReplicasRemoved() != 1 || len(parent.Members()) != 1 || parent.Members()[0] != earlier {
			t.Fatalf("removed=%d members=%v: the later replica must go", f.planner.memo.ReplicasRemoved(), parent.Members())
		}
	})
}

func TestPlanningMergeRefusesGroupsOfDifferentTypes(t *testing.T) {
	t.Parallel()
	f := newPlanningMergeFixture(t)
	survivor := f.group(planningFilter(t, predicates.TriTrue, "Q", f.scan))
	loser := f.group(planningFilter(t, predicates.TriFalse, "Q", f.scan))
	loser.Insert(mustFullUnorderedScan(t, []string{"U"}, values.NotNullString))
	replica := planningFilter(t, predicates.TriTrue, "Q", f.scan)
	loser.Insert(replica)
	f.planner.integratePlanningYield(loser, replica)
	var mismatch *values.ResolutionError
	if !errors.As(f.planner.capErr, &mismatch) || mismatch.ErrorCode != values.MemoResultTypeMismatch {
		t.Fatalf("capErr=%v, want a result-type mismatch", f.planner.capErr)
	}
	if f.planner.memo.MergeCount() != 0 || loser.Canonical() != loser || len(survivor.Members()) != 1 {
		t.Fatal("a refused merge must change nothing")
	}
}

func TestForwardedExploreGroupTaskLeavesSurvivorRoundOpen(t *testing.T) {
	t.Parallel()
	f := newPlanningMergeFixture(t)
	survivor := f.group(planningFilter(t, predicates.TriTrue, "Q", f.scan))
	loser := f.group(planningFilter(t, predicates.TriFalse, "Q", f.scan))
	explored(survivor, loser)
	Set(f.planner.constraintMap, survivor, RequestedOrderingConstraintKey,
		[]*properties.RequestedOrdering{properties.PreserveOrdering()})
	survivor.StartExploration()
	f.yield(t, loser, planningFilter(t, predicates.TriTrue, "Q", f.scan))
	if loser.Canonical() != survivor || !survivor.ConstraintsMap().IsExploring() {
		t.Fatal("precondition: the survivor's round is in flight")
	}
	(&ExploreGroupTask{Phase: PhasePlanning, Ref: loser}).Run(context.Background(), f.planner)
	if !survivor.ConstraintsMap().IsExploring() {
		t.Fatal("a task for the merged-away group committed the survivor's round in flight")
	}
}

// mergeYieldRule yields a fixed expression from any filter.
type mergeYieldRule struct {
	yield expressions.RelationalExpression
}

func (r *mergeYieldRule) Matcher() matching.BindingMatcher {
	return NewExpressionMatcher[*expressions.LogicalFilterExpression]("merge_yield")
}
func (r *mergeYieldRule) OnMatch(call *ExpressionRuleCall) { call.Yield(r.yield) }

func TestPlanningYieldPathsMerge(t *testing.T) {
	t.Parallel()
	t.Run("rule yield", func(t *testing.T) {
		t.Parallel()
		f := newPlanningMergeFixture(t)
		survivor := f.group(planningFilter(t, predicates.TriTrue, "Q", f.scan))
		loser := f.group(planningFilter(t, predicates.TriFalse, "Q", f.scan))
		explored(survivor, loser)
		replica := planningFilter(t, predicates.TriTrue, "Q", f.scan)
		(&TransformExprTask{Phase: PhasePlanning, Ref: loser, Expr: loser.Members()[0], Rule: &mergeYieldRule{yield: replica}}).
			Run(context.Background(), f.planner)
		if f.planner.capErr != nil {
			t.Fatal(f.planner.capErr)
		}
		if f.planner.memo.MergeCount() != 1 || loser.Canonical() != survivor {
			t.Fatal("a PLANNING rule yield found in another group must merge the groups")
		}
		for _, task := range f.planner.stack {
			if task, ok := task.(*ExploreExprTask); ok && task.Expr == replica {
				t.Fatal("the merged-away replica was scheduled for exploration")
			}
		}
	})
	t.Run("data access compensation", func(t *testing.T) {
		t.Parallel()
		f := newPlanningMergeFixture(t)
		survivor := f.group(planningFilter(t, predicates.TriTrue, "Q", f.scan))
		loser := f.group(planningFilter(t, predicates.TriFalse, "Q", f.scan))
		explored(survivor, loser)
		replica := planningFilter(t, predicates.TriTrue, "Q", f.scan)
		f.planner.yieldUnknown(loser, replica)
		if f.planner.memo.MergeCount() != 1 || loser.Canonical() != survivor {
			t.Fatal("a logical data-access compensation found in another group must merge the groups")
		}
		for _, task := range f.planner.stack {
			if task, ok := task.(*ExploreExprTask); ok && task.Expr == replica {
				t.Fatal("the merged-away replica was scheduled for exploration")
			}
		}
	})
}

// Merging changes which groups the search visits, never the plan: the join
// corpus keeps its plans with fewer tasks, and the counts pin that the
// merges keep firing.
func TestPlanningMergeKeepsJoinPlans(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		build  func() expressions.RelationalExpression
		merges int
	}{
		{"chain3", func() expressions.RelationalExpression { return buildOrdinalChainSelect(t, 3) }, 2},
		{"chain4", func() expressions.RelationalExpression { return buildOrdinalChainSelect(t, 4) }, 6},
		{"chain5", func() expressions.RelationalExpression { return buildOrdinalChainSelect(t, 5) }, 40},
		{"star3", func() expressions.RelationalExpression { return buildOrdinalStar(t, 3) }, 6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			plan := func(merge bool) (uint64, int, int) {
				p := fullChainPlanner()
				p.withoutPlanningMerges = !merge
				expr, tasks, err := p.Plan(expressions.InitialOf(tc.build()))
				if err != nil {
					t.Fatal(err)
				}
				physical, ok := expr.(interface{ GetRecordQueryPlan() plans.RecordQueryPlan })
				if !ok {
					t.Fatalf("planned %T has no physical plan", expr)
				}
				return plans.PlanHash(physical.GetRecordQueryPlan()), tasks, p.Memo().MergeCount()
			}
			controlHash, controlTasks, controlMerges := plan(false)
			hash, tasks, merges := plan(true)
			if controlMerges != 0 || merges != tc.merges {
				t.Fatalf("merges=%d (control %d), want %d", merges, controlMerges, tc.merges)
			}
			if hash != controlHash {
				t.Fatalf("plan changed: hash %d, control %d", hash, controlHash)
			}
			if (merges > 0) != (tasks < controlTasks) || tasks > controlTasks {
				t.Fatalf("tasks=%d control=%d: merging must remove work exactly when it fires", tasks, controlTasks)
			}
		})
	}
}
