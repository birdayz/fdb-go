package cascades

import (
	"errors"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func prunedScan(t *testing.T, table string) expressions.RelationalExpression {
	t.Helper()
	return mustFullUnorderedScan(t, []string{table}, &values.RecordType{Fields: []values.Field{
		{Name: "ID", Ordinal: 0, FieldType: values.NullableLong},
	}})
}

// Java's Reference.advancePlannerStage verifies exactly one final: REWRITING
// pruned the group to its winner, which seeds PLANNING. A group with none or
// with several is refused, not carried.
func TestRewritingCrossing_RequiresExactlyOneFinal(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		group  func() *expressions.Reference
		finals int
	}{
		{name: "one final crosses", finals: 1, group: func() *expressions.Reference {
			return expressions.FinalOfAtStage(prunedScan(t, "T"), expressions.StageCanonical)
		}},
		{name: "no final", finals: 0, group: func() *expressions.Reference {
			return expressions.InitialOf(prunedScan(t, "T"))
		}},
		{name: "two finals", finals: 2, group: func() *expressions.Reference {
			ref := expressions.FinalOfAtStage(prunedScan(t, "T"), expressions.StageCanonical)
			ref.InsertFinal(prunedScan(t, "U"))
			return ref
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ref := test.group()
			p := NewPlanner(nil, EmptyPlanContext())
			p.constraintMap = NewConstraintMap()
			(&ExploreGroupTask{Phase: PhasePlanning, Ref: ref}).Run(t.Context(), p)
			var crossing *RewritingCrossingError
			if test.finals == 1 {
				if p.capErr != nil || ref.Stage() != expressions.StagePlanned {
					t.Fatalf("one final: err=%v stage=%v, want a crossing to PLANNED", p.capErr, ref.Stage())
				}
				return
			}
			if !errors.As(p.capErr, &crossing) || crossing.Finals != test.finals {
				t.Fatalf("err = %v, want RewritingCrossingError{Finals: %d}", p.capErr, test.finals)
			}
			if ref.Stage() != expressions.StageCanonical {
				t.Fatalf("a refused group advanced to %v", ref.Stage())
			}
		})
	}
}

// The REWRITING properties descend through each child's ONE final (Java's
// ExpressionCountProperty.forReference: Verify(size() == 1)). The planner's
// comparator refuses any other child; the package-level comparator also reads
// a client-built InitialOf child.
func TestRewritingComparator_ChildMustHaveOneFinal(t *testing.T) {
	t.Parallel()
	over := func(child *expressions.Reference) expressions.RelationalExpression {
		sort, err := expressions.NewLogicalSortExpression(nil, expressions.ForEachQuantifier(child))
		return mustConstruct(t, expressions.RelationalExpression(sort), err)
	}
	twoFinals := expressions.FinalOfAtStage(prunedScan(t, "T"), expressions.StageCanonical)
	twoFinals.InsertFinal(prunedScan(t, "U"))
	oneFinal := expressions.FinalOfAtStage(prunedScan(t, "T"), expressions.StageCanonical)
	clientBuilt := expressions.InitialOf(prunedScan(t, "T"))

	var prune *RewritingPruneError
	planner := &rewritingComparator{}
	planner.compare(over(twoFinals), over(oneFinal))
	if !errors.As(planner.err, &prune) || prune.Finals != 2 {
		t.Fatalf("two-final child: err = %v, want RewritingPruneError{Finals: 2}", planner.err)
	}
	planner = &rewritingComparator{}
	planner.compare(over(clientBuilt), over(oneFinal))
	if !errors.As(planner.err, &prune) || prune.Finals != 0 {
		t.Fatalf("planner comparator over an InitialOf child: err = %v, want RewritingPruneError{Finals: 0}", planner.err)
	}

	client := &rewritingComparator{clientTrees: true}
	if got := client.exprCount(over(clientBuilt), nil, map[*expressions.Reference]bool{}); got != 2 || client.err != nil {
		t.Fatalf("client comparator over an InitialOf child counted %d nodes (err %v), want 2", got, client.err)
	}
	planner = &rewritingComparator{}
	if got := planner.exprCount(over(oneFinal), nil, map[*expressions.Reference]bool{}); got != 2 || planner.err != nil {
		t.Fatalf("planner comparator over a one-final child counted %d nodes (err %v), want 2", got, planner.err)
	}
}

// A memo merge folds a group into an older one. When the survivor never
// explored, no rule ever ran on ITS members, so it must not inherit the
// loser's exploration progress: it keeps the loser's constraints and still
// needs its first exploration (the asymmetric-union no-plan shape).
func TestAbsorb_NeverExploredSurvivorStillNeedsExploration(t *testing.T) {
	t.Parallel()
	survivor := expressions.InitialOf(prunedScan(t, "T"))
	loser := expressions.InitialOf(prunedScan(t, "T"))
	constraints := NewConstraintMap()
	ordering := []*properties.RequestedOrdering{properties.PreserveOrdering()}
	Set(constraints, loser, RequestedOrderingConstraintKey, ordering)
	loser.StartExploration()
	loser.CommitExploration()
	if loser.NeedsExploration() {
		t.Fatal("precondition: the loser's exploration is committed")
	}

	survivor.Absorb(loser)
	if !survivor.NeedsExploration() {
		t.Fatal("a never-explored survivor inherited the loser's exploration progress")
	}
	if _, ok := survivor.ConstraintsMap().GetConstraint(RequestedOrderingConstraintKey); !ok {
		t.Fatal("the survivor lost the loser's pushed constraint")
	}
}
