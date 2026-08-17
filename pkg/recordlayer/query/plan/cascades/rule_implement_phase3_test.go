package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// ---------------------------------------------------------------------------
// 1. ImplementTempTableScanRule
// ---------------------------------------------------------------------------

func TestImplementTempTableScan_PlannerProducesPhysicalScan(t *testing.T) {
	t.Parallel()

	alias := values.NamedCorrelationIdentifier("tt")
	scan := mustTempTableScan(t, alias, values.NotNullLong)
	ref := expressions.InitialOf(scan)

	rules := DefaultExpressionRules()
	p := NewPlanner(rules, EmptyPlanContext()).
		WithPlanningExpressionRules(BatchAExpressionRules()).
		WithImplementationRules(DefaultImplementationRules())
	plan, _, err := p.Plan(ref)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan == nil {
		t.Fatal("Plan returned nil")
	}

	// RFC-184 W2: the temp-table scan is its own bare plan expression.
	rqp, ok := plan.(*plans.RecordQueryTempTableScanPlan)
	if !ok {
		t.Fatalf("plan = %T, want *plans.RecordQueryTempTableScanPlan", plan)
	}
	if rqp.GetTempTableAlias() != alias {
		t.Fatalf("alias = %v, want %v", rqp.GetTempTableAlias(), alias)
	}
}

func TestImplementTempTableScan_ExplainNotEmpty(t *testing.T) {
	t.Parallel()

	alias := values.NamedCorrelationIdentifier("tt_explain")
	scan := mustTempTableScan(t, alias, values.NotNullLong)
	ref := expressions.InitialOf(scan)

	rules := DefaultExpressionRules()
	p := NewPlanner(rules, EmptyPlanContext()).
		WithPlanningExpressionRules(BatchAExpressionRules()).
		WithImplementationRules(DefaultImplementationRules())
	plan, _, err := p.Plan(ref)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan == nil {
		t.Fatal("Plan returned nil")
	}

	explain := ExplainPhysicalPlan(plan)
	if explain == "" {
		t.Fatal("ExplainPhysicalPlan returned empty string")
	}
	t.Logf("TempTableScan Explain: %s", explain)
}

// ---------------------------------------------------------------------------
// 2. ImplementTempTableInsertRule
// ---------------------------------------------------------------------------

func TestImplementTempTableInsert_Fires(t *testing.T) {
	t.Parallel()

	scan := mustFullUnorderedScan(t, []string{"T"}, values.NotNullLong)
	innerRef := expressions.InitialOf(scan)
	alias := values.NamedCorrelationIdentifier("tti")

	insert := mustTempTableInsert(t, expressions.ForEachQuantifier(innerRef), alias, true)
	ref := expressions.InitialOf(insert)

	rules := DefaultExpressionRules()
	p := NewPlanner(rules, EmptyPlanContext()).
		WithPlanningExpressionRules(BatchAExpressionRules()).
		WithImplementationRules(DefaultImplementationRules())
	plan, _, err := p.Plan(ref)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan == nil {
		t.Fatal("Plan returned nil")
	}

	// RFC-184 W2: the temp-table insert is its own bare plan expression.
	rqp, ok := plan.(*plans.RecordQueryTempTableInsertPlan)
	if !ok {
		t.Fatalf("plan = %T, want *plans.RecordQueryTempTableInsertPlan", plan)
	}
	if rqp.GetTempTableAlias() != alias {
		t.Fatalf("alias = %v, want %v", rqp.GetTempTableAlias(), alias)
	}
	if !rqp.IsOwning() {
		t.Fatal("expected owning=true")
	}
	if _, ok := rqp.GetInner().(*plans.RecordQueryScanPlan); !ok {
		t.Fatalf("inner plan = %T, want *RecordQueryScanPlan", rqp.GetInner())
	}
}

func TestImplementTempTableInsert_OwningFlag(t *testing.T) {
	t.Parallel()

	for _, owning := range []bool{true, false} {
		owning := owning
		name := "owning_true"
		if !owning {
			name = "owning_false"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			scan := mustFullUnorderedScan(t, []string{"T"}, values.NotNullLong)
			innerRef := expressions.InitialOf(scan)
			alias := values.NamedCorrelationIdentifier("tti_own_" + name)

			insert := mustTempTableInsert(t, expressions.ForEachQuantifier(innerRef), alias, owning)
			ref := expressions.InitialOf(insert)

			rules := DefaultExpressionRules()
			p := NewPlanner(rules, EmptyPlanContext()).
				WithPlanningExpressionRules(BatchAExpressionRules()).
				WithImplementationRules(DefaultImplementationRules())
			plan, _, err := p.Plan(ref)
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			if plan == nil {
				t.Fatal("Plan returned nil")
			}

			rqp, ok := plan.(*plans.RecordQueryTempTableInsertPlan)
			if !ok {
				t.Fatalf("plan = %T, want *plans.RecordQueryTempTableInsertPlan", plan)
			}
			if rqp.IsOwning() != owning {
				t.Fatalf("IsOwning() = %v, want %v", rqp.IsOwning(), owning)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Helper: build a RecursiveUnionExpression tree with the given strategy.
//
// Structure:
//   Seed leg:      FullUnorderedScan("Seed") -> ref -> TempTableInsert(ForEach(ref), insertAlias, true) -> ref -> ForEach
//   Recursive leg: FullUnorderedScan("Step") -> ref -> TempTableInsert(ForEach(ref), insertAlias, false) -> ref -> ForEach
//   Top:           RecursiveUnionExpression(seedQ, recQ, scanAlias, insertAlias, strategy)
// ---------------------------------------------------------------------------

func buildRecursiveUnionTree(t testing.TB, strategy expressions.TraversalStrategy) *expressions.Reference {
	t.Helper()
	scanAlias := values.NamedCorrelationIdentifier("tt_scan_" + strategy.String())
	insertAlias := values.NamedCorrelationIdentifier("tt_insert_" + strategy.String())

	// Seed leg.
	seedScan := mustFullUnorderedScan(t, []string{"Seed"}, values.NotNullLong)
	seedScanRef := expressions.InitialOf(seedScan)
	seedInsert := mustTempTableInsert(t, expressions.ForEachQuantifier(seedScanRef), insertAlias, true)
	seedRef := expressions.InitialOf(seedInsert)

	// Recursive leg.
	recScan := mustFullUnorderedScan(t, []string{"Step"}, values.NotNullLong)
	recScanRef := expressions.InitialOf(recScan)
	recInsert := mustTempTableInsert(t, expressions.ForEachQuantifier(recScanRef), insertAlias, false)
	recRef := expressions.InitialOf(recInsert)

	recUnion := mustRecursiveUnion(t,
		expressions.ForEachQuantifier(seedRef),
		expressions.ForEachQuantifier(recRef),
		scanAlias,
		insertAlias,
		strategy,
	)
	return expressions.InitialOf(recUnion)
}

// ---------------------------------------------------------------------------
// 3. ImplementRecursiveDfsJoinRule
// ---------------------------------------------------------------------------

func TestImplementRecursiveDfsJoin_Fires_PreorderStrategy(t *testing.T) {
	t.Parallel()

	ref := buildRecursiveUnionTree(t, expressions.TraversalPreorder)

	rules := DefaultExpressionRules()
	p := NewPlanner(rules, EmptyPlanContext()).
		WithPlanningExpressionRules(BatchAExpressionRules()).
		WithImplementationRules(DefaultImplementationRules())
	if _, _, err := p.Plan(ref); err != nil {
		t.Fatalf("Plan: %v", err)
	}

	var dfsWrap *plans.RecordQueryRecursiveDfsJoinPlan
	for _, m := range ref.AllMembers() {
		if w, ok := m.(*plans.RecordQueryRecursiveDfsJoinPlan); ok {
			dfsWrap = w
			break
		}
	}
	if dfsWrap == nil {
		t.Fatal("planner did not produce a physical RecursiveDfsJoin member for TraversalPreorder")
	}

	plan, ok := dfsWrap.GetRecordQueryPlan().(*plans.RecordQueryRecursiveDfsJoinPlan)
	if !ok {
		t.Fatalf("GetRecordQueryPlan() = %T, want *RecordQueryRecursiveDfsJoinPlan", dfsWrap.GetRecordQueryPlan())
	}
	if plan.GetTraversalStrategy() != plans.DfsPreorder {
		t.Fatalf("strategy = %v, want DfsPreorder", plan.GetTraversalStrategy())
	}
}

func TestImplementRecursiveDfsJoin_Fires_AnyStrategy(t *testing.T) {
	t.Parallel()

	ref := buildRecursiveUnionTree(t, expressions.TraversalAny)

	rules := DefaultExpressionRules()
	p := NewPlanner(rules, EmptyPlanContext()).
		WithPlanningExpressionRules(BatchAExpressionRules()).
		WithImplementationRules(DefaultImplementationRules())
	if _, _, err := p.Plan(ref); err != nil {
		t.Fatalf("Plan: %v", err)
	}

	foundDfs := false
	for _, m := range ref.AllMembers() {
		if IsPhysicalRecursiveDfsJoin(m) {
			foundDfs = true
			break
		}
	}
	if !foundDfs {
		t.Fatal("TraversalAny should allow DFS join, but no physical RecursiveDfsJoin found")
	}
}

func TestImplementRecursiveDfsJoin_Declines_LevelStrategy(t *testing.T) {
	t.Parallel()

	ref := buildRecursiveUnionTree(t, expressions.TraversalLevel)

	rules := DefaultExpressionRules()
	p := NewPlanner(rules, EmptyPlanContext()).
		WithPlanningExpressionRules(BatchAExpressionRules()).
		WithImplementationRules(DefaultImplementationRules())
	if _, _, err := p.Plan(ref); err != nil {
		t.Fatalf("Plan: %v", err)
	}

	for _, m := range ref.AllMembers() {
		if IsPhysicalRecursiveDfsJoin(m) {
			t.Fatal("TraversalLevel should NOT produce a DFS join, but one was found")
		}
	}

	// Level union rule should fire instead.
	foundLevel := false
	for _, m := range ref.AllMembers() {
		if IsPhysicalRecursiveLevelUnion(m) {
			foundLevel = true
			break
		}
	}
	if !foundLevel {
		t.Fatal("TraversalLevel should produce a LevelUnion plan, but none found")
	}
}

// ---------------------------------------------------------------------------
// 4. ImplementRecursiveLevelUnionRule
// ---------------------------------------------------------------------------

func TestImplementRecursiveLevelUnion_Fires_LevelStrategy(t *testing.T) {
	t.Parallel()

	ref := buildRecursiveUnionTree(t, expressions.TraversalLevel)

	rules := DefaultExpressionRules()
	p := NewPlanner(rules, EmptyPlanContext()).
		WithPlanningExpressionRules(BatchAExpressionRules()).
		WithImplementationRules(DefaultImplementationRules())
	if _, _, err := p.Plan(ref); err != nil {
		t.Fatalf("Plan: %v", err)
	}

	var wrap *plans.RecordQueryRecursiveLevelUnionPlan
	for _, m := range ref.AllMembers() {
		if w, ok := m.(*plans.RecordQueryRecursiveLevelUnionPlan); ok {
			wrap = w
			break
		}
	}
	if wrap == nil {
		t.Fatal("planner did not produce a physical RecursiveLevelUnion member for TraversalLevel")
	}

	plan, ok := wrap.GetRecordQueryPlan().(*plans.RecordQueryRecursiveLevelUnionPlan)
	if !ok {
		t.Fatalf("GetRecordQueryPlan() = %T, want *RecordQueryRecursiveLevelUnionPlan", wrap.GetRecordQueryPlan())
	}
	if plan.GetInitialState() == nil {
		t.Fatal("initial state plan is nil")
	}
	if plan.GetRecursiveState() == nil {
		t.Fatal("recursive state plan is nil")
	}

	explain := ExplainPhysicalPlan(wrap)
	if explain == "" {
		t.Fatal("ExplainPhysicalPlan returned empty")
	}
	t.Logf("RecursiveLevelUnion Explain: %s", explain)
}

func TestImplementRecursiveLevelUnion_Fires_AnyStrategy(t *testing.T) {
	t.Parallel()

	// FORMATION: the level-union rule itself must fire for TraversalAny.
	// The full-planner run below can no longer show the level union
	// alongside the DFS join — physical yields land ONLY in FinalMembers
	// and OptimizeGroup prunes finals to the winner (Java's prune-to-1),
	// so the losing alternative is not visible in AllMembers() after
	// Plan(). Fire the rule directly on a prepared tree to pin formation.
	topRef, isr, ir, rsr, rr := buildLevelUnionTree(t, expressions.TraversalAny)
	implementInnerCTEPlans(t, isr, ir, rsr, rr)
	yielded := mustFireExpressionRule(t, NewImplementRecursiveLevelUnionRule(), topRef)
	if len(yielded) != 1 || !IsPhysicalRecursiveLevelUnion(yielded[0]) {
		t.Fatalf("ImplementRecursiveLevelUnionRule must yield the level union for TraversalAny; got %d yields", len(yielded))
	}

	// WINNER: the full planner must keep SOME physical recursive-union
	// implementation (level union or DFS join — whichever the cost model
	// picks) as the surviving final.
	ref := buildRecursiveUnionTree(t, expressions.TraversalAny)

	rules := DefaultExpressionRules()
	p := NewPlanner(rules, EmptyPlanContext()).
		WithPlanningExpressionRules(BatchAExpressionRules()).
		WithImplementationRules(DefaultImplementationRules())
	if _, _, err := p.Plan(ref); err != nil {
		t.Fatalf("Plan: %v", err)
	}

	foundImpl := false
	for _, m := range ref.AllMembers() {
		if IsPhysicalRecursiveLevelUnion(m) || IsPhysicalRecursiveDfsJoin(m) {
			foundImpl = true
			break
		}
	}
	if !foundImpl {
		t.Fatal("TraversalAny should produce a physical recursive-union winner (level union or DFS join), found neither")
	}
}
