package cascades

import (
	"context"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

type stagedPhysicalYieldRule struct {
	matcher matching.BindingMatcher
	yield   expressions.RelationalExpression
}

func newStagedPhysicalYieldRule(yield expressions.RelationalExpression) *stagedPhysicalYieldRule {
	return &stagedPhysicalYieldRule{
		matcher: NewExpressionMatcher[*scanPlanExpression]("staged_physical_yield"),
		yield:   yield,
	}
}

func (r *stagedPhysicalYieldRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *stagedPhysicalYieldRule) OnMatch(call *ExpressionRuleCall) {
	call.Yield(r.yield)
}

func physicalYieldFixture(t testing.TB, recordType string, rowType values.Type) *scanPlanExpression {
	t.Helper()
	plan, err := plans.NewRecordQueryScanPlan([]string{recordType}, rowType, false)
	if err != nil {
		t.Fatalf("construct %s physical scan: %v", recordType, err)
	}
	return &scanPlanExpression{plan: plan}
}

func TestTransformExprSchedulesOnlyCommittedNewYield(t *testing.T) {
	t.Parallel()

	rowType := values.NewRecordType("ROW", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
	})

	t.Run("deduped", func(t *testing.T) {
		existing := physicalYieldFixture(t, "T", rowType)
		duplicate := physicalYieldFixture(t, "T", rowType)
		ref := expressions.FinalOfAtStage(existing, expressions.StagePlanned)
		planner := NewPlanner(nil, EmptyPlanContext())

		(&TransformExprTask{
			Phase: PhasePlanning,
			Ref:   ref,
			Expr:  existing,
			Rule:  newStagedPhysicalYieldRule(duplicate),
		}).Run(context.Background(), planner)

		if planner.capErr != nil {
			t.Fatalf("deduped rule yield failed: %v", planner.capErr)
		}
		if got := len(ref.FinalMembers()); got != 1 {
			t.Fatalf("deduped yield grew final members to %d, want 1", got)
		}
		if got := len(planner.stack); got != 0 {
			t.Fatalf("deduped yield scheduled %d downstream tasks, want none", got)
		}
	})

	t.Run("inserted", func(t *testing.T) {
		existing := physicalYieldFixture(t, "T", rowType)
		inserted := physicalYieldFixture(t, "U", rowType)
		ref := expressions.FinalOfAtStage(existing, expressions.StagePlanned)
		planner := NewPlanner(nil, EmptyPlanContext())

		(&TransformExprTask{
			Phase: PhasePlanning,
			Ref:   ref,
			Expr:  existing,
			Rule:  newStagedPhysicalYieldRule(inserted),
		}).Run(context.Background(), planner)

		if planner.capErr != nil {
			t.Fatalf("inserted rule yield failed: %v", planner.capErr)
		}
		if got := len(ref.FinalMembers()); got != 2 {
			t.Fatalf("new yield left %d final members, want 2", got)
		}
		if got := len(planner.stack); got != 2 {
			t.Fatalf("new physical yield scheduled %d downstream tasks, want explore+optimize", got)
		}
		var explore, optimize int
		for _, task := range planner.stack {
			switch task.(type) {
			case *ExploreExprTask:
				explore++
			case *OptimizeInputsTask:
				optimize++
			default:
				t.Fatalf("new physical yield scheduled unexpected task %T", task)
			}
		}
		if explore != 1 || optimize != 1 {
			t.Fatalf("new physical yield scheduled explore=%d optimize=%d, want 1 each", explore, optimize)
		}
	})
}

var _ ExpressionRule = (*stagedPhysicalYieldRule)(nil)
