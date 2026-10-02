package cascades

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
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
		inserted, err := plans.NewRecordQueryLimitPlan(physicalYieldFixture(t, "U", rowType).plan, 1, 0)
		if err != nil {
			t.Fatal(err)
		}
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

func TestTransformExprIndexesCommittedPlanningYield(t *testing.T) {
	t.Parallel()
	existing := physicalYieldFixture(t, "T", values.NotNullLong)
	ref := expressions.FinalOf(existing)
	planner := NewPlanner(nil, EmptyPlanContext())
	planner.memo = NewMemo(ref)
	planner.memo.MarkPlanningActive()
	child := planner.memo.MemoizeExpression(memoTestScan(t, "T"))
	inserted, err := expressions.NewLogicalDistinctExpression(expressions.ForEachQuantifier(child))
	inserted = mustConstruct(t, inserted, err)
	task := &TransformMatchPartitionTask{TransformExprTask: TransformExprTask{
		Phase: PhasePlanning, Ref: ref, Expr: existing, Rule: newStagedPhysicalYieldRule(inserted),
	}}
	task.Run(context.Background(), planner)
	if planner.capErr != nil {
		t.Fatal(planner.capErr)
	}
	if !ref.ContainsExactly(inserted) {
		t.Fatal("logical yield did not commit")
	}
	candidates := slices.Collect(planner.memo.findCandidateParents(inserted.GetQuantifiers(), nil))
	if len(candidates) != 1 || candidates[0] != ref {
		t.Errorf("committed logical yield missing from planning topology: %v", candidates)
	}
	if got := planner.memo.MemoizeExpression(inserted); got != ref {
		t.Error("committed planning alternative was not reused")
	}
	if planner.memo.MergeCount() != 0 {
		t.Fatal("planning indexing merged groups")
	}
}

func TestPredicateNormalizationIsPlanningOnly(t *testing.T) {
	t.Parallel()
	if _, ok := LookupRule("NormalizePredicatesRule").(*NormalizePredicatesRule); !ok {
		t.Error("planning normalizer must remain discoverable in the rule registry")
	}
	planner := fullChainPlanner()
	for _, phase := range []PlannerPhase{PhaseRewriting, PhasePlanning} {
		rules, _ := planner.rulesForPhase(phase)
		normalizers := 0
		for _, rule := range rules {
			if _, ok := rule.(*NormalizePredicatesRule); ok {
				normalizers++
			}
		}
		want := 0
		if phase == PhasePlanning {
			want = 1
		}
		if normalizers != want {
			t.Errorf("phase %v has %d CNF normalizers, want %d; CNF is an access-path alternative, not a canonical rewrite", phase, normalizers, want)
		}
	}
}

func TestPlanningNormalizationRemainsExploratory(t *testing.T) {
	t.Parallel()
	for _, inputs := range []int{1, 2} {
		t.Run(fmt.Sprintf("inputs=%d", inputs), func(t *testing.T) {
			t.Parallel()
			var qs []expressions.Quantifier
			for i := range inputs {
				scan := mustFullUnorderedScan(t, []string{fmt.Sprintf("T%d", i)}, filterRuleRowType())
				qs = append(qs, expressions.ForEachQuantifier(expressions.ExploratoryOfAtStage(scan, expressions.StagePlanned)))
			}
			row, err := qs[0].RequireFlowedObjectValue()
			row = mustConstruct(t, row, err)
			a, b, c := predAt(row, "a"), predAt(row, "b"), predAt(row, "c")
			sel, err := expressions.NewSelectExpression(row, qs, []predicates.QueryPredicate{predicates.NewOr(predicates.NewAnd(a, b), c)})
			sel = mustConstruct(t, sel, err)
			ref := expressions.ExploratoryOfAtStage(sel, expressions.StagePlanned)
			planner := NewPlanner(nil, EmptyPlanContext())
			planner.memo = NewMemo(ref)
			planner.memo.MarkPlanningActive()
			task := &TransformExprTask{Phase: PhasePlanning, Ref: ref, Expr: sel, Rule: NewNormalizePredicatesRule()}
			task.Run(context.Background(), planner)
			if planner.capErr != nil {
				t.Fatal(planner.capErr)
			}
			if len(ref.Members()) != 2 || len(ref.FinalMembers()) != 0 {
				t.Errorf("normalization yielded exploratory=%d final=%d, want 2 and 0", len(ref.Members()), len(ref.FinalMembers()))
			}
			normalized := sel.WithPredicates([]predicates.QueryPredicate{predicates.NewOr(a, c), predicates.NewOr(b, c)})
			before := len(planner.memo.refs)
			if got := planner.memo.MemoizeExpression(normalized); got != ref || len(planner.memo.refs) != before {
				t.Error("memoization failed to reuse the normalized planning alternative")
			}
			if len(planner.stack) != 1 {
				t.Fatalf("normalization scheduled %d tasks, want one exploration", len(planner.stack))
			}
			if _, ok := planner.stack[0].(*ExploreExprTask); !ok {
				t.Fatalf("logical yield scheduled %T, want exploration only", planner.stack[0])
			}
			task.Run(context.Background(), planner)
			if planner.capErr != nil || len(planner.stack) != 1 {
				t.Fatal("duplicate normalization scheduled work or failed")
			}
		})
	}
}

func TestDataAccessYieldIndexesOnlyCommittedMembers(t *testing.T) {
	t.Parallel()
	for _, lane := range []string{"logical", "physical"} {
		t.Run(lane, func(t *testing.T) {
			t.Parallel()
			root := expressions.ExploratoryOfAtStage(memoTestScan(t, "T"), expressions.StagePlanned)
			planner := NewPlanner(nil, EmptyPlanContext())
			planner.memo = NewMemo(root)
			planner.memo.MarkPlanningActive()
			var committed expressions.RelationalExpression
			for i := range 2 {
				child := expressions.FinalOfAtStage(physicalYieldFixture(t, "T", values.NotNullLong), expressions.StagePlanned)
				q := expressions.ForEachQuantifier(child)
				var yielded expressions.RelationalExpression
				if lane == "logical" {
					expr, err := expressions.NewLogicalDistinctExpression(q)
					yielded = mustConstruct(t, expr, err)
				} else {
					plan, err := plans.NewRecordQueryUnorderedPrimaryKeyDistinctPlanFromQuantifier(q)
					yielded = mustConstruct(t, plan, err)
				}
				planner.yieldUnknown(root, yielded)
				if i == 0 {
					committed = yielded
				}
				if root.ContainsExactly(yielded) != (i == 0) {
					t.Fatal("fixture must commit the first yield and discard its duplicate")
				}
				if planner.memo.ContainsReference(child) != (i == 0) {
					t.Errorf("yield %d: child indexed=%v, want %v", i, planner.memo.ContainsReference(child), i == 0)
				}
				indexed := 0
				for _, edge := range planner.memo.childToParents[child] {
					if edge.parent == root && edge.expr == yielded {
						indexed++
					}
				}
				want := 1 - i
				if indexed != want {
					t.Errorf("yield %d: topology edges=%d, want %d", i, indexed, want)
				}
			}
			if !root.ContainsExactly(committed) || planner.memo.MergeCount() != 0 {
				t.Fatal("data-access indexing lost its committed member or merged planning groups")
			}
		})
	}
}

type topologyImplementationRule struct {
	matcher     matching.BindingMatcher
	swappedOnly bool
	yielded     []expressions.RelationalExpression
}

func (r *topologyImplementationRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *topologyImplementationRule) OnMatch(call *ImplementationRuleCall) {
	sel := matching.Get[*expressions.SelectExpression](call.Bindings, r.matcher)
	if sel.IsQuantifiersSwapped() != r.swappedOnly {
		return
	}
	for range 2 {
		plan, err := plans.NewRecordQueryUnorderedPrimaryKeyDistinctPlanFromQuantifier(sel.GetQuantifiers()[0])
		if err != nil {
			call.Fail(err)
			return
		}
		r.yielded = append(r.yielded, plan)
		call.Yield(plan)
	}
}

func TestImplementationYieldIndexesOnlyCommittedMembers(t *testing.T) {
	t.Parallel()
	for _, driver := range []string{"task", "standalone"} {
		for _, arm := range []string{"normal", "swapped"} {
			t.Run(driver+"/"+arm, func(t *testing.T) {
				t.Parallel()
				left := expressions.ForEachQuantifier(expressions.FinalOf(physicalYieldFixture(t, "T", values.NotNullLong)))
				right := expressions.ForEachQuantifier(expressions.FinalOf(physicalYieldFixture(t, "U", values.NotNullLong)))
				result, err := left.RequireFlowedObjectValue()
				result = mustConstruct(t, result, err)
				sel, err := expressions.NewSelectExpression(result, []expressions.Quantifier{left, right}, nil)
				sel = mustConstruct(t, sel, err)
				ref := expressions.ExploratoryOfAtStage(sel, expressions.StagePlanned)
				planner := NewPlanner(nil, EmptyPlanContext())
				planner.memo = NewMemo(ref)
				planner.memo.MarkPlanningActive()
				rule := &topologyImplementationRule{
					matcher: NewExpressionMatcher[*expressions.SelectExpression]("topology_yield"), swappedOnly: arm == "swapped",
				}
				if driver == "task" {
					(&TransformImplTask{Phase: PhasePlanning, Ref: ref, Expr: sel, Rule: rule}).Run(context.Background(), planner)
					if planner.capErr != nil {
						t.Fatal(planner.capErr)
					}
				} else if _, err := FireImplementationRuleWithContext(rule, ref, EmptyPlanContext(), planner.memo, NewConstraintMap()); err != nil {
					t.Fatal(err)
				}
				if len(rule.yielded) != 2 || len(ref.FinalMembers()) != 1 {
					t.Fatalf("yielded=%d committed=%d; fixture must include one duplicate", len(rule.yielded), len(ref.FinalMembers()))
				}
				for i, yielded := range rule.yielded {
					indexed := 0
					for _, edge := range planner.memo.childToParents[yielded.GetQuantifiers()[0].GetRangesOver()] {
						if edge.parent == ref && edge.expr == yielded {
							indexed++
						}
					}
					want := 0
					if i == 0 {
						want = 1
					}
					if indexed != want {
						t.Errorf("yield %d: topology edges=%d, want %d (only committed members belong in the index)", i, indexed, want)
					}
				}
				if planner.memo.MergeCount() != 0 {
					t.Fatal("implementation yield merged planning groups")
				}
			})
		}
	}
}

var _ ExpressionRule = (*stagedPhysicalYieldRule)(nil)
