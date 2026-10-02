package cascades

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
)

// growingTask registers one fresh group, the growth a rule's memoization makes.
type growingTask struct{}

func (growingTask) Run(_ context.Context, p *Planner) {
	p.memo.RegisterReference(plannerLifecycleTestRef())
}

func TestPlannerTraceAttributesTasksRulesAndGroups(t *testing.T) {
	t.Parallel()
	trace := NewPlannerTrace()
	planner := plannerLifecycleTestPlanner().WithTrace(trace)
	planner.memo = NewMemo(plannerLifecycleTestRef())
	trace.attach(planner.memo)
	planner.activePhase = PhasePlanning
	rule := NewPredicateToLogicalUnionRule()
	ref := plannerLifecycleTestRef()
	for _, task := range []Task{growingTask{}, growingTask{}, &TransformExprTask{Ref: ref, Rule: rule}, &TransformMatchPartitionTask{TransformExprTask{Ref: ref, Rule: rule}}} {
		planner.runTraced(context.Background(), task)
	}
	got := trace.Entries()
	want := []PlannerTraceEntry{
		{Phase: PhasePlanning, Kind: "growingTask", Tasks: 2, NewGroups: 2},
		{Phase: PhasePlanning, Kind: "TransformExprTask", Rule: "PredicateToLogicalUnionRule", Tasks: 1},
		{Phase: PhasePlanning, Kind: "TransformMatchPartitionTask", Rule: "PredicateToLogicalUnionRule", Tasks: 1},
	}
	if trace.Tasks() != 4 || trace.InitialGroups() != 1 || len(got) != len(want) {
		t.Fatalf("tasks=%d initial=%d rows=%+v", trace.Tasks(), trace.InitialGroups(), got)
	}
	for _, w := range want {
		if !slices.ContainsFunc(got, func(g PlannerTraceEntry) bool {
			return g.Phase == w.Phase && g.Kind == w.Kind && g.Rule == w.Rule && g.Tasks == w.Tasks && g.NewGroups == w.NewGroups
		}) {
			t.Fatalf("rows %+v miss %+v", got, w)
		}
	}
}

// The census compares members over the same child groups: two groups holding
// one operator over one child are found, and so are two equal leaves; parents of
// two equivalent but distinct children are not compared.
func TestPlannerTraceCensusFindsEquivalentGroups(t *testing.T) {
	t.Parallel()
	unique := func(child *expressions.Reference) *expressions.Reference {
		return expressions.InitialOf(mustLifecycleConstruct(expressions.NewLogicalUniqueExpression(expressions.ForEachQuantifier(child))))
	}
	for _, tc := range []struct {
		name string
		// build returns the second union input and the groups the census must
		// report as one equivalent class, if any.
		build func(first, child *expressions.Reference) (*expressions.Reference, []*expressions.Reference)
	}{
		{"same operator over the same child", func(first, child *expressions.Reference) (*expressions.Reference, []*expressions.Reference) {
			second := unique(child)
			return second, []*expressions.Reference{first, second}
		}},
		{"equal leaves under distinct parents", func(_, child *expressions.Reference) (*expressions.Reference, []*expressions.Reference) {
			other := plannerLifecycleTestRef()
			return unique(other), []*expressions.Reference{child, other}
		}},
		{"other operator", func(_, child *expressions.Reference) (*expressions.Reference, []*expressions.Reference) {
			return expressions.InitialOf(mustLifecycleConstruct(expressions.NewLogicalUnionExpression([]expressions.Quantifier{expressions.ForEachQuantifier(child)}))), nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			child := plannerLifecycleTestRef()
			first := unique(child)
			second, want := tc.build(first, child)
			root := expressions.InitialOf(mustLifecycleConstruct(expressions.NewLogicalUnionExpression([]expressions.Quantifier{
				expressions.ForEachQuantifier(first), expressions.ForEachQuantifier(second),
			})))
			trace := NewPlannerTrace()
			trace.attach(NewMemo(root))
			census := trace.Census(2)
			var got []uint64
			for _, class := range census.EquivalentGroups {
				for _, group := range class {
					got = append(got, group.ID)
				}
			}
			var wantIDs []uint64
			for _, ref := range want {
				wantIDs = append(wantIDs, ref.ID())
			}
			slices.Sort(wantIDs)
			if len(census.EquivalentGroups) > 1 || !slices.Equal(got, wantIDs) {
				t.Fatalf("equivalent classes=%v, want one class of groups %v", census.EquivalentGroups, wantIDs)
			}
			if census.Groups != len(trace.memo.References()) || census.Members["LogicalUnionExpression"] == 0 || len(census.LargestGroups) != 2 {
				t.Fatalf("census=%+v", census)
			}
			var report strings.Builder
			if err := trace.WriteReport(&report, 5); err != nil || !strings.Contains(report.String(), fmt.Sprintf("explored more than once): %d\n", len(census.EquivalentGroups))) {
				t.Fatalf("report=%q err=%v", report.String(), err)
			}
		})
	}
}
