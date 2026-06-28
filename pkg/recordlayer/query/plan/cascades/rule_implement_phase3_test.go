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
	scan := expressions.NewTempTableScanExpression(alias)
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

	wrap, ok := plan.(*physicalTempTableScanWrapper)
	if !ok {
		t.Fatalf("plan = %T, want *physicalTempTableScanWrapper", plan)
	}
	rqp, ok := wrap.GetRecordQueryPlan().(*plans.RecordQueryTempTableScanPlan)
	if !ok {
		t.Fatalf("GetRecordQueryPlan() = %T, want *RecordQueryTempTableScanPlan", wrap.GetRecordQueryPlan())
	}
	if rqp.GetTempTableAlias() != alias {
		t.Fatalf("alias = %v, want %v", rqp.GetTempTableAlias(), alias)
	}
}

func TestImplementTempTableScan_ExplainNotEmpty(t *testing.T) {
	t.Parallel()

	alias := values.NamedCorrelationIdentifier("tt_explain")
	scan := expressions.NewTempTableScanExpression(alias)
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

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	innerRef := expressions.InitialOf(scan)
	alias := values.NamedCorrelationIdentifier("tti")

	insert := expressions.NewTempTableInsertExpression(
		expressions.ForEachQuantifier(innerRef),
		alias,
		true,
	)
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

	wrap, ok := plan.(*physicalTempTableInsertWrapper)
	if !ok {
		t.Fatalf("plan = %T, want *physicalTempTableInsertWrapper", plan)
	}
	rqp, ok := wrap.GetRecordQueryPlan().(*plans.RecordQueryTempTableInsertPlan)
	if !ok {
		t.Fatalf("GetRecordQueryPlan() = %T, want *RecordQueryTempTableInsertPlan", wrap.GetRecordQueryPlan())
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

			scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
			innerRef := expressions.InitialOf(scan)
			alias := values.NamedCorrelationIdentifier("tti_own_" + name)

			insert := expressions.NewTempTableInsertExpression(
				expressions.ForEachQuantifier(innerRef),
				alias,
				owning,
			)
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

			wrap, ok := plan.(*physicalTempTableInsertWrapper)
			if !ok {
				t.Fatalf("plan = %T, want *physicalTempTableInsertWrapper", plan)
			}
			rqp := wrap.GetRecordQueryPlan().(*plans.RecordQueryTempTableInsertPlan)
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

func buildRecursiveUnionTree(strategy expressions.TraversalStrategy) *expressions.Reference {
	scanAlias := values.NamedCorrelationIdentifier("tt_scan_" + strategy.String())
	insertAlias := values.NamedCorrelationIdentifier("tt_insert_" + strategy.String())

	// Seed leg.
	seedScan := expressions.NewFullUnorderedScanExpression([]string{"Seed"}, values.UnknownType)
	seedScanRef := expressions.InitialOf(seedScan)
	seedInsert := expressions.NewTempTableInsertExpression(
		expressions.ForEachQuantifier(seedScanRef),
		insertAlias,
		true,
	)
	seedRef := expressions.InitialOf(seedInsert)

	// Recursive leg.
	recScan := expressions.NewFullUnorderedScanExpression([]string{"Step"}, values.UnknownType)
	recScanRef := expressions.InitialOf(recScan)
	recInsert := expressions.NewTempTableInsertExpression(
		expressions.ForEachQuantifier(recScanRef),
		insertAlias,
		false,
	)
	recRef := expressions.InitialOf(recInsert)

	recUnion := expressions.NewRecursiveUnionExpression(
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

	ref := buildRecursiveUnionTree(expressions.TraversalPreorder)

	rules := DefaultExpressionRules()
	p := NewPlanner(rules, EmptyPlanContext()).
		WithPlanningExpressionRules(BatchAExpressionRules()).
		WithImplementationRules(DefaultImplementationRules())
	if _, _, err := p.Plan(ref); err != nil {
		t.Fatalf("Plan: %v", err)
	}

	var dfsWrap *physicalRecursiveDfsJoinWrapper
	for _, m := range ref.AllMembers() {
		if w, ok := m.(*physicalRecursiveDfsJoinWrapper); ok {
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

	ref := buildRecursiveUnionTree(expressions.TraversalAny)

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

	ref := buildRecursiveUnionTree(expressions.TraversalLevel)

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

	ref := buildRecursiveUnionTree(expressions.TraversalLevel)

	rules := DefaultExpressionRules()
	p := NewPlanner(rules, EmptyPlanContext()).
		WithPlanningExpressionRules(BatchAExpressionRules()).
		WithImplementationRules(DefaultImplementationRules())
	if _, _, err := p.Plan(ref); err != nil {
		t.Fatalf("Plan: %v", err)
	}

	var wrap *physicalRecursiveLevelUnionWrapper
	for _, m := range ref.AllMembers() {
		if w, ok := m.(*physicalRecursiveLevelUnionWrapper); ok {
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

	ref := buildRecursiveUnionTree(expressions.TraversalAny)

	rules := DefaultExpressionRules()
	p := NewPlanner(rules, EmptyPlanContext()).
		WithPlanningExpressionRules(BatchAExpressionRules()).
		WithImplementationRules(DefaultImplementationRules())
	if _, _, err := p.Plan(ref); err != nil {
		t.Fatalf("Plan: %v", err)
	}

	foundLevel := false
	for _, m := range ref.AllMembers() {
		if IsPhysicalRecursiveLevelUnion(m) {
			foundLevel = true
			break
		}
	}
	if !foundLevel {
		t.Fatal("TraversalAny should allow level union, but no physical RecursiveLevelUnion found")
	}

	// TraversalAny should also produce DFS as an alternative.
	foundDfs := false
	for _, m := range ref.AllMembers() {
		if IsPhysicalRecursiveDfsJoin(m) {
			foundDfs = true
			break
		}
	}
	if !foundDfs {
		t.Fatal("TraversalAny should produce both DFS and Level alternatives; DFS missing")
	}
}
