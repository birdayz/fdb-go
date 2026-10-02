package cascades

import (
	"context"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

type inputlessOrdinalExpression struct{ *plans.RecordQueryLimitPlan }

func (e *inputlessOrdinalExpression) GetQuantifiers() []expressions.Quantifier { return nil }

func TestOptimizeInputsSchedulingRetainsValidation(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		kind int
	}{
		{"logical_leaf", 0},
		{"physical_leaf", 1},
		{"physical_parent", 2},
		{"malformed_leaf", 3},
		{"malformed_sibling", 4},
		{"physical_sibling", 5},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			scan := mustOrderedScanPlan(t, []string{"T"}, false)
			limit, err := plans.NewRecordQueryLimitPlan(scan, 1, 0)
			if err != nil {
				t.Fatal(err)
			}
			malformed := &inputlessOrdinalExpression{limit}
			var expr expressions.RelationalExpression = scan
			switch test.kind {
			case 0:
				expr = mustOrderedScanFull(t, []string{"T"})
			case 2:
				expr = limit
			case 3:
				expr = malformed
			}
			ref := expressions.FinalOf(expr)
			if test.kind == 4 {
				ref.InsertFinal(malformed)
			} else if test.kind == 5 {
				ref.InsertFinal(limit)
			}
			p := NewPlanner(nil, nil)
			p.constraintMap = NewConstraintMap()
			p.push(&OptimizeInputsTask{Phase: PhasePlanning, Ref: ref, Expr: expr})
			want := 0
			if test.kind >= 2 {
				want = 1
			}
			if got := len(p.stack); got != want {
				t.Fatalf("queued %d OptimizeInputs tasks, want %d", got, want)
			}
			if want == 0 {
				return
			}
			p.pop().Run(t.Context(), p)
			if test.kind == 3 || test.kind == 4 {
				if p.capErr == nil {
					t.Fatal("malformed zero-quantifier ordinal requirements were not rejected")
				}
			} else {
				if p.capErr != nil {
					t.Fatal(p.capErr)
				}
				prunes := 0
				for _, task := range p.stack {
					if _, ok := task.(*OptimizeGroupTask); ok {
						prunes++
					}
				}
				wantPrunes := 1
				if test.kind == 5 {
					wantPrunes = 0
				}
				if prunes != wantPrunes {
					t.Fatalf("scheduled %d child optimizations, want %d", prunes, wantPrunes)
				}
				child := limit.GetQuantifiers()[0].GetRangesOver()
				if requirements, ok := Get(p.constraintMap, child, OrdinalLayoutConstraintKey); !ok || len(requirements) != 1 {
					t.Fatalf("child requirements = %v, want the physical parent's retained layout", requirements)
				}
			}
		})
	}
}

func TestOptimizeInputsCoalescesAdjacentLeafPrepasses(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"adjacent", "intervening_task", "different_group", "forwarded_group", "dead_parent", "dead_leaf", "both_dead", "malformed_leaf", "valid", "cancelled", "other_phase", "second_leaf_survives"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			parent, child, _, _ := ordinalLayoutSelectionFixture(t)
			leaf := child.Winner()
			ref := expressions.FinalOf(parent)
			ref.InsertFinal(leaf)
			p := NewPlanner(nil, EmptyPlanContext())
			p.constraintMap = NewConstraintMap()
			phase := PhasePlanning
			if mode == "other_phase" {
				phase = PhaseRewriting
			}
			p.push(&OptimizeInputsTask{Phase: phase, Ref: ref, Expr: parent})
			leafRef := ref
			if mode == "different_group" || mode == "forwarded_group" {
				leafRef = expressions.FinalOf(parent)
				leafRef.InsertFinal(leaf)
				if mode == "forwarded_group" {
					ref.Absorb(leafRef)
				}
			}
			if mode == "intervening_task" {
				p.push(&InitiatePlannerPhaseTask{Phase: PhasePlanning, RootRef: ref})
			}
			if mode == "malformed_leaf" {
				leaf = &inputlessOrdinalExpression{parent}
				ref.InsertFinal(leaf)
			}
			p.push(&OptimizeInputsTask{Phase: PhasePlanning, Ref: leafRef, Expr: leaf})
			if mode == "second_leaf_survives" {
				second := mustPushFetchConstruct(plans.NewRecordQueryScanPlan([]string{"SECOND"}, parent.GetResultType(), false))
				ref.InsertFinal(second)
				p.push(&OptimizeInputsTask{Phase: PhasePlanning, Ref: ref, Expr: second})
				ref.PruneToSet(map[expressions.RelationalExpression]struct{}{second: {}})
			}
			want := 1
			if mode == "different_group" || mode == "malformed_leaf" || mode == "other_phase" {
				want = 2
			} else if mode == "intervening_task" {
				want = 3
			}
			if len(p.stack) != want {
				t.Fatalf("queued %d tasks, want %d", len(p.stack), want)
			}
			if want != 1 {
				return
			}
			// A surviving leaf must still trigger the group prepass if the
			// adjacent parent is removed before their shared task runs.
			if mode == "dead_parent" {
				ref.PruneToSet(map[expressions.RelationalExpression]struct{}{leaf: {}})
			} else if mode == "dead_leaf" {
				ref.PruneToSet(map[expressions.RelationalExpression]struct{}{parent: {}})
			} else if mode == "both_dead" {
				ref.PruneToSet(nil)
			}
			if mode != "valid" {
				ref.InsertFinal(&inputlessOrdinalExpression{parent})
			}
			ctx := t.Context()
			if mode == "cancelled" {
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
			}
			p.pop().Run(ctx, p)
			wantError := mode != "both_dead" && mode != "valid" && mode != "cancelled"
			if (p.capErr != nil) != wantError {
				t.Fatalf("shared input prepass error=%v, want error=%t", p.capErr, wantError)
			}
			if mode == "valid" {
				if requirements, ok := Get(p.constraintMap, child, OrdinalLayoutConstraintKey); !ok || len(requirements) != 1 {
					t.Fatalf("shared prepass lost the parent's ordinal requirement: %v", requirements)
				}
				prunes := 0
				for _, task := range p.stack {
					if optimize, ok := task.(*OptimizeGroupTask); ok && optimize.Ref == child {
						prunes++
					}
				}
				if prunes != 1 {
					t.Fatalf("shared task scheduled %d child optimizers, want 1", prunes)
				}
			}
		})
	}
}

func TestRulelessInputClosureRetainsOrdinalFailures(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"incompatible_descendant", "malformed_input", "incompatible_sibling", "malformed_sibling"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			parent, child, _, _ := ordinalLayoutSelectionFixture(t)
			var inner expressions.RelationalExpression = parent
			if mode == "malformed_input" || mode == "malformed_sibling" {
				inner = &inputlessOrdinalExpression{parent}
			}
			inputRef := expressions.FinalOf(inner)
			var outer expressions.RelationalExpression = mustPushFetchConstruct(plans.NewRecordQueryPredicatesFilterPlanFromQuantifier(
				expressions.NewPhysicalQuantifier(inputRef), nil))
			var untouched *expressions.Reference
			if mode == "incompatible_sibling" || mode == "malformed_sibling" {
				untouched = expressions.FinalOf(mustPushFetchConstruct(plans.NewRecordQueryScanPlan(
					[]string{"UNTOUCHED"}, parent.GetResultType(), false)))
				outer = mustPushFetchConstruct(plans.NewRecordQueryUnorderedUnionPlanFromQuantifiers([]expressions.Quantifier{
					expressions.NewPhysicalQuantifier(untouched), expressions.NewPhysicalQuantifier(inputRef), expressions.NewPhysicalQuantifier(untouched),
				}))
			}
			child.PruneToSet(map[expressions.RelationalExpression]struct{}{child.Winner(): {}})
			child.AdvanceStagePreservingMembers(expressions.StagePlanned)
			child.ConstraintsMap().SetExplored()
			ref := expressions.FinalOf(outer)
			p := NewPlanner(nil, EmptyPlanContext())
			p.constraintMap = NewConstraintMap()
			p.planningExpressionRules = []ExpressionRule{NewMatchLeafRule(), NewMatchIntermediateRule()}
			tick := child.ConstraintsMap().CurrentTick()
			p.push(&OptimizeInputsTask{Phase: PhasePlanning, Ref: ref, Expr: outer})
			(&ExploreExprTask{Phase: PhasePlanning, Ref: ref, Expr: outer}).Run(t.Context(), p)
			for len(p.stack) > 0 && p.capErr == nil {
				p.pop().Run(t.Context(), p)
			}
			if p.capErr == nil {
				t.Fatal("ruleless input completion bypassed ordinal validation")
			}
			if untouched != nil && untouched.ConstraintsMap().IsExplored() {
				t.Fatal("sibling completion did not preserve reverse first-occurrence dependency order")
			}
			if child.ConstraintsMap().CurrentTick() != tick {
				t.Fatal("optimizer-only validation rearmed expression exploration")
			}
		})
	}
}

func TestRulelessGroupCompletionStillValidatesOrdinalRequirements(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"late_requirement", "malformed_sibling"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			parent, child, _, _ := ordinalLayoutSelectionFixture(t)
			member := child.Winner()
			if mode == "malformed_sibling" {
				member = child.FinalMembers()[0]
			}
			child.PruneToSet(map[expressions.RelationalExpression]struct{}{member: {}})
			child.AdvanceStagePreservingMembers(expressions.StagePlanned)
			child.ConstraintsMap().SetExplored()
			ref := expressions.FinalOf(parent)
			if mode == "malformed_sibling" {
				ref.InsertFinal(&inputlessOrdinalExpression{parent})
			}
			p := NewPlanner(nil, EmptyPlanContext())
			p.constraintMap = NewConstraintMap()
			tick := child.ConstraintsMap().CurrentTick()
			(&ExploreGroupTask{Phase: PhasePlanning, Ref: ref}).Run(t.Context(), p)
			for len(p.stack) > 0 && p.capErr == nil {
				p.pop().Run(t.Context(), p)
			}
			if p.capErr == nil {
				t.Fatal("ruleless group completion bypassed ordinal validation")
			}
			if child.ConstraintsMap().CurrentTick() != tick {
				t.Fatal("optimizer-only validation rearmed expression exploration")
			}
		})
	}
}

func TestSettledSingletonInputsStillValidateOrdinalRequirements(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"late_requirement", "malformed_sibling"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			parent, child, _, _ := ordinalLayoutSelectionFixture(t)
			member := child.Winner()
			if mode == "malformed_sibling" {
				member = child.FinalMembers()[0]
			}
			child.PruneToSet(map[expressions.RelationalExpression]struct{}{member: {}})
			child.AdvanceStagePreservingMembers(expressions.StagePlanned)
			child.ConstraintsMap().SetExplored()
			ref := expressions.FinalOf(parent)
			if mode == "malformed_sibling" {
				ref.InsertFinal(&inputlessOrdinalExpression{parent})
			}
			p := NewPlanner(nil, EmptyPlanContext())
			p.constraintMap = NewConstraintMap()
			tick := child.ConstraintsMap().CurrentTick()
			(&OptimizeInputsTask{Phase: PhasePlanning, Ref: ref, Expr: parent}).Run(t.Context(), p)
			for len(p.stack) > 0 && p.capErr == nil {
				p.pop().Run(t.Context(), p)
			}
			if p.capErr == nil {
				t.Fatal("settled singleton bypassed ordinal validation")
			}
			if child.ConstraintsMap().CurrentTick() != tick {
				t.Fatal("optimizer-only validation rearmed expression exploration")
			}
		})
	}
}

// TestExtractionFiltersOrdinalLayoutBeforeCost is the mutation pin for the
// physical-property selection boundary. The Limit is finalized while its live
// child reference contains only a retained-window join, so it requires that
// exact layout. The same reference then gains a cheaper identity-layout scan
// and OPTIMIZE stamps that incompatible scan as its global winner. Extraction
// must ignore the cheaper winner for this parent and select from the compatible
// subset before comparing cost.
//
// Removing the requirement-aware branch in either extraction path makes the
// selector case relink the Limit directly to incompatibleScan (via Winner), and
// makes the selector-less case choose it through CostLess.
func TestExtractionFiltersOrdinalLayoutBeforeCost(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name    string
		extract func(*expressions.Reference) (expressions.RelationalExpression, error)
	}{
		{
			name: "selector winner",
			extract: func(ref *expressions.Reference) (expressions.RelationalExpression, error) {
				return ExtractBestPlanFromSelector(ref, nil, properties.DefaultStatistics{})
			},
		},
		{
			name: "selector-less cost fold",
			extract: func(ref *expressions.Reference) (expressions.RelationalExpression, error) {
				return ExtractBestPlan(ref)
			},
		},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			parent, childRef, compatibleLayout, incompatibleLayout := ordinalLayoutSelectionFixture(t)

			if !properties.CostLess(childRef.Winner(), compatiblePhysicalMember(t, childRef, compatibleLayout)) {
				t.Fatal("fixture: stamped incompatible winner is not cheaper than the compatible join")
			}

			extracted, err := test.extract(expressions.FinalOf(parent))
			if err != nil {
				t.Fatalf("extract: %v", err)
			}
			limit, ok := extracted.(*plans.RecordQueryLimitPlan)
			if !ok {
				t.Fatalf("extracted = %T, want *plans.RecordQueryLimitPlan", extracted)
			}
			inner := limit.GetInner()
			if inner == nil {
				t.Fatal("extracted Limit has no inner plan")
			}
			gotLayout, err := inner.ProvidedOutputLayout()
			if err != nil {
				t.Fatalf("inner ProvidedOutputLayout: %v", err)
			}
			if !gotLayout.RawEqual(compatibleLayout) {
				t.Fatal("extraction selected a child outside the parent requirement")
			}
			if gotLayout.RawEqual(incompatibleLayout) {
				t.Fatal("extraction selected the cheaper incompatible identity layout")
			}

			physicalProperties, err := limit.OrdinalPhysicalProperties()
			if err != nil {
				t.Fatalf("extracted Limit properties: %v", err)
			}
			requirements := physicalProperties.RequiredInputLayouts()
			if len(requirements) != 1 {
				t.Fatalf("extracted Limit requirements = %d, want 1", len(requirements))
			}
			if satisfied, satisfyErr := requirements[0].SatisfiedBy(gotLayout); satisfyErr != nil || !satisfied {
				t.Fatalf("extracted requirement satisfaction = (%v,%v), want true,nil", satisfied, satisfyErr)
			}
		})
	}
}

func TestOrdinalLayoutRequirementDoesNotRestartExploration(t *testing.T) {
	t.Parallel()
	parent, child, _, _ := ordinalLayoutSelectionFixture(t)
	requirements, err := ordinalInputRequirementsOf(parent)
	if err != nil || len(requirements) != 1 {
		t.Fatalf("requirements: %v, %v", requirements, err)
	}
	child.ConstraintsMap().SetExplored()
	constraints := NewConstraintMap()
	if !Set(constraints, child, OrdinalLayoutConstraintKey, requirements) {
		t.Fatal("new optimization requirement was not stored")
	}
	if child.NeedsExploration() {
		t.Fatal("optimizer-only layout requirement restarted expression rules")
	}
	stored, ok := Get(constraints, child, OrdinalLayoutConstraintKey)
	if !ok || len(stored) != 1 {
		t.Fatal("optimizer lost the required layout")
	}
	if Set(constraints, child, OrdinalLayoutConstraintKey, requirements) {
		t.Fatal("identical requirement was not subsumed")
	}
	otherParent, err := plans.NewRecordQueryLimitPlanFromQuantifier(
		expressions.NewPhysicalQuantifier(child), 2, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	otherRequirements, err := ordinalInputRequirementsOf(otherParent)
	if err != nil {
		t.Fatal(err)
	}
	if !Set(constraints, child, OrdinalLayoutConstraintKey, otherRequirements) {
		t.Fatal("distinct layout requirement was not accumulated")
	}
	stored, ok = Get(constraints, child, OrdinalLayoutConstraintKey)
	if !ok || len(stored) != 2 || child.NeedsExploration() {
		t.Fatal("growing retention requirements lost a layout or restarted expression rules")
	}
	child.ConstraintsMap().ReArm()
	if !child.NeedsExploration() {
		t.Fatal("member growth must still restart exploration")
	}
	child.ConstraintsMap().SetExplored()
	ordinary := &PlannerConstraint[int]{name: "ordinary"}
	Set(constraints, child, ordinary, 1)
	if !child.NeedsExploration() {
		t.Fatal("ordinary constraint growth must still restart exploration")
	}
}

func TestExtractionFailsClosedWhenCompatibleLayoutWasPruned(t *testing.T) {
	t.Parallel()
	parent, childRef, _, _ := ordinalLayoutSelectionFixture(t)
	incompatible := childRef.Winner()
	childRef.PruneToSet(map[expressions.RelationalExpression]struct{}{incompatible: {}})
	requirements, requirementErr := ordinalInputRequirementsOf(parent)
	if requirementErr != nil || len(requirements) != 1 {
		t.Fatalf("parent requirements = (%d,%v), want 1,nil", len(requirements), requirementErr)
	}
	planner := NewPlanner(nil, nil)
	planner.constraintMap = NewConstraintMap()
	Set(planner.constraintMap, childRef, OrdinalLayoutConstraintKey, requirements)
	(&OptimizeGroupTask{Phase: PhasePlanning, Ref: childRef}).Run(context.Background(), planner)
	if planner.capErr == nil {
		t.Fatal("OptimizeGroup accepted a required layout with no compatible final")
	}

	extracted, err := ExtractBestPlanFromSelector(
		expressions.FinalOf(parent), nil, properties.DefaultStatistics{})
	if err == nil || extracted != nil {
		t.Fatalf("extraction after compatible-member pruning = (%T,%v), want nil,error", extracted, err)
	}
}

// TestOptimizeInputsAndGroupRetainWinnersPerOrdinalLayout pins the planner-side
// half of requirement-aware extraction. Parent A is finalized while the child
// group exposes only layout A. The group is then mutated with a cheaper layout
// B and a costlier second provider of layout A, and parent B is finalized
// against B. Both parents push into the same child constraint; OptimizeGroup
// must keep the global B winner plus the cheapest A-compatible winner, while
// pruning the costlier A provider. A last-write-wins constraint loses A, and a
// cost fold performed before compatibility keeps the wrong member.
func TestOptimizeInputsAndGroupRetainWinnersPerOrdinalLayout(t *testing.T) {
	t.Parallel()

	parentA, childRef, layoutA, layoutB := ordinalLayoutSelectionFixture(t)
	memberA := compatiblePhysicalMember(t, childRef, layoutA)
	memberB := childRef.Winner()
	if memberB == nil {
		t.Fatal("fixture: layout-B global winner is nil")
	}

	holderA, ok := memberA.(physicalPlanHolder)
	if !ok || holderA.GetRecordQueryPlan() == nil {
		t.Fatalf("fixture: layout-A member %T is not a physical plan", memberA)
	}
	// DISTINCT is an actual pass-through provider: it preserves the selected
	// child's exact source-window layout while adding work. InMemorySort used to
	// serve this fixture, but it is now correctly a materialization boundary and
	// publishes a fresh current-only layout, so it cannot represent a second
	// provider of layout A.
	expensiveA, err := plans.NewRecordQueryDistinctPlan(holderA.GetRecordQueryPlan())
	if err != nil {
		t.Fatalf("costlier layout-A provider: %v", err)
	}
	expensiveLayout, err := expensiveA.ProvidedOutputLayout()
	if err != nil {
		t.Fatalf("costlier layout-A provider layout: %v", err)
	}
	if !expensiveLayout.RawEqual(layoutA) {
		t.Fatal("fixture: pass-through distinct did not preserve layout A")
	}
	if !childRef.InsertFinal(expensiveA) {
		t.Fatal("fixture: costlier layout-A provider deduplicated")
	}
	if !PlanningCostModelLess(memberA, expensiveA) {
		t.Fatal("fixture: base layout-A provider must be cheaper than its distinct wrapper")
	}
	if !PlanningCostModelLess(memberB, memberA) {
		t.Fatal("fixture: layout-B scan must be the global cost winner")
	}
	// Inserting the costlier A alternative correctly invalidates the previously
	// stamped winner. Re-stamp B to model the completed cost fold whose selected
	// layout parent B is supposed to capture.
	childRef.SetWinner(memberB)

	// The child winner was changed to B after parent A was built, so a second
	// parent over the same live edge captures a genuinely different exact
	// requirement. This is the mutation that makes a property-blind winner
	// unsafe for parent A.
	parentB, err := plans.NewRecordQueryLimitPlanFromQuantifier(
		expressions.NewPhysicalQuantifier(childRef), 2, 0, nil)
	if err != nil {
		t.Fatalf("layout-B parent: %v", err)
	}
	requirementsA, err := ordinalInputRequirementsOf(parentA)
	if err != nil || len(requirementsA) != 1 {
		t.Fatalf("parent A requirements = (%d,%v), want 1,nil", len(requirementsA), err)
	}
	requirementsB, err := ordinalInputRequirementsOf(parentB)
	if err != nil || len(requirementsB) != 1 {
		t.Fatalf("parent B requirements = (%d,%v), want 1,nil", len(requirementsB), err)
	}
	if ok, satisfyErr := requirementsA[0].SatisfiedBy(layoutB); satisfyErr != nil || ok {
		t.Fatalf("parent A unexpectedly accepts layout B: (%v,%v)", ok, satisfyErr)
	}
	if ok, satisfyErr := requirementsB[0].SatisfiedBy(layoutA); satisfyErr != nil || ok {
		t.Fatalf("parent B unexpectedly accepts layout A: (%v,%v)", ok, satisfyErr)
	}

	p := NewPlanner(nil, nil)
	p.constraintMap = NewConstraintMap()
	parentRef := expressions.FinalOf(parentA)
	if !parentRef.InsertFinal(parentB) {
		t.Fatal("fixture: distinct layout-B parent deduplicated from layout-A parent")
	}
	// The leaf shares B's prepass; both parent layouts must still be published
	// before B's child pruning, regardless of sibling insertion order.
	parentRef.InsertFinal(memberB)
	p.push(&OptimizeInputsTask{Phase: PhasePlanning, Ref: parentRef, Expr: parentB})
	p.push(&OptimizeInputsTask{Phase: PhasePlanning, Ref: parentRef, Expr: memberB})
	if len(p.stack) != 1 {
		t.Fatalf("adjacent leaf prepass retained %d tasks, want 1", len(p.stack))
	}
	p.pop().Run(t.Context(), p)
	tickAfterBoth := childRef.ConstraintsMap().CurrentTick()
	// An identical re-push is subsumed; it must not grow the exploration
	// epoch or duplicate retention work.
	(&OptimizeInputsTask{
		Phase: PhasePlanning, Ref: parentRef, Expr: parentB,
	}).Run(context.Background(), p)
	if tick := childRef.ConstraintsMap().CurrentTick(); tick != tickAfterBoth {
		t.Fatalf("duplicate layout requirement changed constraint tick: %d -> %d", tickAfterBoth, tick)
	}
	if p.capErr != nil {
		t.Fatalf("OptimizeInputs: %v", p.capErr)
	}
	constraints, ok := Get(p.constraintMap, childRef, OrdinalLayoutConstraintKey)
	if !ok || len(constraints) != 2 {
		t.Fatalf("shared child ordinal constraints = %d (ok=%v), want 2", len(constraints), ok)
	}

	(&OptimizeGroupTask{Phase: PhasePlanning, Ref: childRef}).Run(context.Background(), p)
	if p.capErr != nil {
		t.Fatalf("OptimizeGroup: %v", p.capErr)
	}
	if got := childRef.Winner(); got != memberB {
		t.Fatalf("global winner = %T, want cheaper layout-B member %T", got, memberB)
	}
	if got := len(childRef.FinalMembers()); got != 2 {
		t.Fatalf("retained finals = %d, want one winner for each of two layouts", got)
	}
	if childRef.ContainsExactly(expensiveA) {
		t.Fatal("costlier provider of layout A survived property-keyed pruning")
	}

	bestA, err := bestOrdinalCompatiblePhysicalMember(
		childRef, requirementsA[0], PlanningCostModelLess)
	if err != nil || bestA != memberA {
		t.Fatalf("layout-A property winner = (%T,%v), want cheapest compatible %T", bestA, err, memberA)
	}
	bestB, err := bestOrdinalCompatiblePhysicalMember(
		childRef, requirementsB[0], PlanningCostModelLess)
	if err != nil || bestB != memberB {
		t.Fatalf("layout-B property winner = (%T,%v), want global compatible %T", bestB, err, memberB)
	}
}

func ordinalLayoutSelectionFixture(
	t testing.TB,
) (*plans.RecordQueryLimitPlan, *expressions.Reference, values.OrdinalLayout, values.OrdinalLayout) {
	t.Helper()
	outerType := values.NewRecordType("selection_outer", false, []values.Field{
		{Name: "OUTER_VALUE", Ordinal: 0, FieldType: values.NotNullLong},
	})
	innerType := values.NewRecordType("selection_inner", false, []values.Field{
		{Name: "INNER_VALUE", Ordinal: 0, FieldType: values.NotNullLong},
	})
	outerAlias := values.NamedCorrelationIdentifier("SELECTION_OUTER")
	innerAlias := values.NamedCorrelationIdentifier("SELECTION_INNER")
	outerSource := mustOrdinalSelectionQOV(t, outerAlias, outerType)
	innerSource := mustOrdinalSelectionQOV(t, innerAlias, innerType)
	outerField, err := values.ResolveOrdinalSeedField(outerSource, 0)
	if err != nil {
		t.Fatalf("outer seed field: %v", err)
	}
	innerField, err := values.ResolveOrdinalSeedField(innerSource, 0)
	if err != nil {
		t.Fatalf("inner seed field: %v", err)
	}
	result := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "OUTER_VALUE", Value: outerField},
		values.RecordConstructorField{Name: "INNER_VALUE", Value: innerField},
	)
	outerPlan, err := plans.NewRecordQueryScanPlan([]string{"OUTER"}, outerType, false)
	if err != nil {
		t.Fatalf("outer plan: %v", err)
	}
	innerPlan, err := plans.NewRecordQueryScanPlan([]string{"INNER"}, innerType, false)
	if err != nil {
		t.Fatalf("inner plan: %v", err)
	}
	compatible, err := plans.NewRecordQueryNestedLoopJoinPlanFromQuantifiers(
		expressions.NamedForEachQuantifier(outerAlias, expressions.FinalOf(outerPlan)),
		expressions.NamedForEachQuantifier(innerAlias, expressions.FinalOf(innerPlan)),
		nil, plans.JoinInner, outerAlias, innerAlias, result,
	)
	if err != nil {
		t.Fatalf("compatible retained-window join: %v", err)
	}
	compatibleLayout, err := compatible.ProvidedOutputLayout()
	if err != nil {
		t.Fatalf("compatible layout: %v", err)
	}
	incompatible, err := plans.NewRecordQueryScanPlan(
		[]string{"CHEAP_IDENTITY"}, compatible.GetResultType(), false)
	if err != nil {
		t.Fatalf("incompatible identity scan: %v", err)
	}
	incompatibleLayout, err := incompatible.ProvidedOutputLayout()
	if err != nil {
		t.Fatalf("incompatible layout: %v", err)
	}
	if compatibleLayout.RawEqual(incompatibleLayout) {
		t.Fatal("fixture: retained-window and identity layouts are equal")
	}

	childRef := expressions.FinalOfAtStage(compatible, expressions.StageCanonical)
	parent, err := plans.NewRecordQueryLimitPlanFromQuantifier(
		expressions.NewPhysicalQuantifier(childRef), 1, 0, nil)
	if err != nil {
		t.Fatalf("parent Limit: %v", err)
	}
	parentProperties, err := parent.OrdinalPhysicalProperties()
	if err != nil {
		t.Fatalf("parent properties: %v", err)
	}
	requirements := parentProperties.RequiredInputLayouts()
	if len(requirements) != 1 {
		t.Fatalf("fixture: parent requirements = %d, want 1", len(requirements))
	}
	if satisfied, satisfyErr := requirements[0].SatisfiedBy(incompatibleLayout); satisfyErr != nil || satisfied {
		t.Fatalf("fixture: incompatible satisfaction = (%v,%v), want false,nil", satisfied, satisfyErr)
	}

	if !childRef.InsertFinal(incompatible) {
		t.Fatal("fixture: incompatible alternative deduplicated")
	}
	childRef.SetWinner(incompatible)
	return parent, childRef, compatibleLayout, incompatibleLayout
}

func compatiblePhysicalMember(
	t testing.TB,
	ref *expressions.Reference,
	want values.OrdinalLayout,
) expressions.RelationalExpression {
	t.Helper()
	for _, member := range ref.AllMembers() {
		holder, ok := member.(physicalPlanHolder)
		if !ok || holder.GetRecordQueryPlan() == nil {
			continue
		}
		layout, err := holder.GetRecordQueryPlan().ProvidedOutputLayout()
		if err == nil && layout.RawEqual(want) {
			return member
		}
	}
	t.Fatal("fixture: compatible member is absent")
	return nil
}

func mustOrdinalSelectionQOV(
	t testing.TB,
	alias values.CorrelationIdentifier,
	typ values.Type,
) values.QuantifiedObjectValue {
	t.Helper()
	qov, err := values.NewQuantifiedObjectValue(alias, typ)
	if err != nil {
		t.Fatalf("QOV %s: %v", alias.Name(), err)
	}
	return qov
}
