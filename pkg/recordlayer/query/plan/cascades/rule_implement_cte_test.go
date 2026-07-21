package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// --- ImplementTempTableScanRule ---

func TestImplementTempTableScan_Fires(t *testing.T) {
	t.Parallel()

	alias := values.NamedCorrelationIdentifier("tt_scan")
	scanExpr := expressions.NewTempTableScanExpression(alias)
	ref := expressions.InitialOf(scanExpr)

	yielded := FireExpressionRule(NewImplementTempTableScanRule(), ref)
	if len(yielded) != 1 {
		t.Fatalf("ImplementTempTableScanRule yielded %d, want 1", len(yielded))
	}

	// RFC-184 W2: the temp-table scan is its own bare plan expression.
	plan, ok := yielded[0].(*plans.RecordQueryTempTableScanPlan)
	if !ok {
		t.Fatalf("yield = %T, want *plans.RecordQueryTempTableScanPlan", yielded[0])
	}
	if plan.GetTempTableAlias() != alias {
		t.Fatalf("plan alias = %v, want %v", plan.GetTempTableAlias(), alias)
	}
}

func TestImplementTempTableScan_ViaPlanner(t *testing.T) {
	t.Parallel()

	alias := values.NamedCorrelationIdentifier("tt_scan_planner")
	scanExpr := expressions.NewTempTableScanExpression(alias)
	ref := expressions.InitialOf(scanExpr)

	rules := DefaultExpressionRules()
	p := NewPlanner(rules, EmptyPlanContext()).
		WithPlanningExpressionRules(BatchAExpressionRules()).
		WithImplementationRules(DefaultImplementationRules())
	if _, _, err := p.Plan(ref); err != nil {
		t.Fatalf("Plan: %v", err)
	}

	foundPhysical := false
	for _, m := range ref.AllMembers() {
		if _, ok := m.(*plans.RecordQueryTempTableScanPlan); ok {
			foundPhysical = true
			break
		}
	}
	if !foundPhysical {
		t.Fatal("planner did not produce a physical TempTableScan member")
	}
}

func TestImplementTempTableScan_PlanOutput(t *testing.T) {
	t.Parallel()

	alias := values.NamedCorrelationIdentifier("tt_scan_plan")
	scanExpr := expressions.NewTempTableScanExpression(alias)
	ref := expressions.InitialOf(scanExpr)

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

// --- ImplementTempTableInsertRule ---

func TestImplementTempTableInsert_FiresAfterScanImplemented(t *testing.T) {
	t.Parallel()

	// FullUnorderedScan → Reference → TempTableInsertExpression(ForEach(ref), alias, true)
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	innerRef := expressions.InitialOf(scan)
	alias := values.NamedCorrelationIdentifier("tti_fire")

	insert := expressions.NewTempTableInsertExpression(
		expressions.ForEachQuantifier(innerRef),
		alias,
		true,
	)
	topRef := expressions.InitialOf(insert)

	// Implement the inner scan first (PrimaryScanRule fires on FullUnorderedScan).
	FireExpressionRule(NewPrimaryScanRule(), innerRef)

	yielded := FireExpressionRule(NewImplementTempTableInsertRule(), topRef)
	if len(yielded) != 1 {
		t.Fatalf("ImplementTempTableInsertRule yielded %d, want 1", len(yielded))
	}

	// RFC-184 W2: the temp-table insert is its own bare plan expression.
	plan, ok := yielded[0].(*plans.RecordQueryTempTableInsertPlan)
	if !ok {
		t.Fatalf("yield = %T, want *plans.RecordQueryTempTableInsertPlan", yielded[0])
	}
	if plan.GetTempTableAlias() != alias {
		t.Fatalf("alias = %v, want %v", plan.GetTempTableAlias(), alias)
	}
	if !plan.IsOwning() {
		t.Fatal("plan should be owning")
	}
	inner := plan.GetInner()
	if _, ok := inner.(*plans.RecordQueryScanPlan); !ok {
		t.Fatalf("inner = %T, want *RecordQueryScanPlan", inner)
	}
}

func TestImplementTempTableInsert_NoFireWithoutPhysicalInner(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	innerRef := expressions.InitialOf(scan)

	insert := expressions.NewTempTableInsertExpression(
		expressions.ForEachQuantifier(innerRef),
		values.NamedCorrelationIdentifier("tti_nofire"),
		true,
	)
	topRef := expressions.InitialOf(insert)

	// Do NOT implement the inner scan — rule should not fire.
	yielded := FireExpressionRule(NewImplementTempTableInsertRule(), topRef)
	if len(yielded) != 0 {
		t.Fatalf("ImplementTempTableInsertRule fired without physical inner; yielded %d", len(yielded))
	}
}

func TestImplementTempTableInsert_ViaPlanner(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	innerRef := expressions.InitialOf(scan)
	alias := values.NamedCorrelationIdentifier("tti_planner")

	insert := expressions.NewTempTableInsertExpression(
		expressions.ForEachQuantifier(innerRef),
		alias,
		true,
	)
	topRef := expressions.InitialOf(insert)

	rules := DefaultExpressionRules()
	p := NewPlanner(rules, EmptyPlanContext()).
		WithPlanningExpressionRules(BatchAExpressionRules()).
		WithImplementationRules(DefaultImplementationRules())
	plan, _, err := p.Plan(topRef)
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
}

// --- ImplementRecursiveDfsJoinRule ---

func TestImplementRecursiveDfsJoin_TraversalAny_YieldsPreorder(t *testing.T) {
	t.Parallel()

	scanAlias := values.NamedCorrelationIdentifier("tt_scan_rdj")
	insertAlias := values.NamedCorrelationIdentifier("tt_insert_rdj")

	// Initial leg: TempTableInsert(FullUnorderedScan, alias, true)
	initialScan := expressions.NewFullUnorderedScanExpression([]string{"Seed"}, values.UnknownType)
	initialScanRef := expressions.InitialOf(initialScan)
	initialInsert := expressions.NewTempTableInsertExpression(
		expressions.ForEachQuantifier(initialScanRef),
		insertAlias,
		true,
	)
	initialRef := expressions.InitialOf(initialInsert)

	// Recursive leg: TempTableInsert(FullUnorderedScan, alias, false)
	// (simulating the recursive step — in real use this would scan the temp table)
	recursiveScan := expressions.NewFullUnorderedScanExpression([]string{"Step"}, values.UnknownType)
	recursiveScanRef := expressions.InitialOf(recursiveScan)
	recursiveInsert := expressions.NewTempTableInsertExpression(
		expressions.ForEachQuantifier(recursiveScanRef),
		insertAlias,
		false,
	)
	recursiveRef := expressions.InitialOf(recursiveInsert)

	recUnion := expressions.NewRecursiveUnionExpression(
		expressions.ForEachQuantifier(initialRef),
		expressions.ForEachQuantifier(recursiveRef),
		scanAlias,
		insertAlias,
		expressions.TraversalAny,
	)
	topRef := expressions.InitialOf(recUnion)

	// Implement inner plans first.
	FireExpressionRule(NewPrimaryScanRule(), initialScanRef)
	FireExpressionRule(NewImplementTempTableInsertRule(), initialRef)
	FireExpressionRule(NewPrimaryScanRule(), recursiveScanRef)
	FireExpressionRule(NewImplementTempTableInsertRule(), recursiveRef)

	yielded := FireExpressionRule(NewImplementRecursiveDfsJoinRule(), topRef)
	if len(yielded) != 1 {
		t.Fatalf("ImplementRecursiveDfsJoinRule yielded %d, want 1", len(yielded))
	}

	if !IsPhysicalRecursiveDfsJoin(yielded[0]) {
		t.Fatalf("yield = %T, want *plans.RecordQueryRecursiveDfsJoinPlan", yielded[0])
	}

	wrap := yielded[0].(*plans.RecordQueryRecursiveDfsJoinPlan)
	plan, ok := wrap.GetRecordQueryPlan().(*plans.RecordQueryRecursiveDfsJoinPlan)
	if !ok {
		t.Fatalf("GetRecordQueryPlan() = %T, want *RecordQueryRecursiveDfsJoinPlan", wrap.GetRecordQueryPlan())
	}

	// TraversalAny + PreOrderAllowed → DfsPreorder.
	if plan.GetTraversalStrategy() != plans.DfsPreorder {
		t.Fatalf("strategy = %v, want DfsPreorder", plan.GetTraversalStrategy())
	}
	if plan.GetPriorCorrelation() != scanAlias {
		t.Fatalf("priorCorrelation = %v, want %v", plan.GetPriorCorrelation(), scanAlias)
	}
}

func TestImplementRecursiveDfsJoin_TraversalPostorder(t *testing.T) {
	t.Parallel()

	scanAlias := values.NamedCorrelationIdentifier("tt_scan_post")
	insertAlias := values.NamedCorrelationIdentifier("tt_insert_post")

	initialScan := expressions.NewFullUnorderedScanExpression([]string{"Seed"}, values.UnknownType)
	initialScanRef := expressions.InitialOf(initialScan)
	initialInsert := expressions.NewTempTableInsertExpression(
		expressions.ForEachQuantifier(initialScanRef),
		insertAlias,
		true,
	)
	initialRef := expressions.InitialOf(initialInsert)

	recursiveScan := expressions.NewFullUnorderedScanExpression([]string{"Step"}, values.UnknownType)
	recursiveScanRef := expressions.InitialOf(recursiveScan)
	recursiveInsert := expressions.NewTempTableInsertExpression(
		expressions.ForEachQuantifier(recursiveScanRef),
		insertAlias,
		false,
	)
	recursiveRef := expressions.InitialOf(recursiveInsert)

	recUnion := expressions.NewRecursiveUnionExpression(
		expressions.ForEachQuantifier(initialRef),
		expressions.ForEachQuantifier(recursiveRef),
		scanAlias,
		insertAlias,
		expressions.TraversalPostorder,
	)
	topRef := expressions.InitialOf(recUnion)

	FireExpressionRule(NewPrimaryScanRule(), initialScanRef)
	FireExpressionRule(NewImplementTempTableInsertRule(), initialRef)
	FireExpressionRule(NewPrimaryScanRule(), recursiveScanRef)
	FireExpressionRule(NewImplementTempTableInsertRule(), recursiveRef)

	yielded := FireExpressionRule(NewImplementRecursiveDfsJoinRule(), topRef)
	if len(yielded) != 1 {
		t.Fatalf("ImplementRecursiveDfsJoinRule yielded %d, want 1", len(yielded))
	}

	wrap := yielded[0].(*plans.RecordQueryRecursiveDfsJoinPlan)
	plan := wrap.GetRecordQueryPlan().(*plans.RecordQueryRecursiveDfsJoinPlan)
	if plan.GetTraversalStrategy() != plans.DfsPostorder {
		t.Fatalf("strategy = %v, want DfsPostorder", plan.GetTraversalStrategy())
	}
}

func TestImplementRecursiveDfsJoin_TraversalLevel_DoesNotFire(t *testing.T) {
	t.Parallel()

	scanAlias := values.NamedCorrelationIdentifier("tt_scan_lvl")
	insertAlias := values.NamedCorrelationIdentifier("tt_insert_lvl")

	initialScan := expressions.NewFullUnorderedScanExpression([]string{"Seed"}, values.UnknownType)
	initialScanRef := expressions.InitialOf(initialScan)
	initialInsert := expressions.NewTempTableInsertExpression(
		expressions.ForEachQuantifier(initialScanRef),
		insertAlias,
		true,
	)
	initialRef := expressions.InitialOf(initialInsert)

	recursiveScan := expressions.NewFullUnorderedScanExpression([]string{"Step"}, values.UnknownType)
	recursiveScanRef := expressions.InitialOf(recursiveScan)
	recursiveInsert := expressions.NewTempTableInsertExpression(
		expressions.ForEachQuantifier(recursiveScanRef),
		insertAlias,
		false,
	)
	recursiveRef := expressions.InitialOf(recursiveInsert)

	recUnion := expressions.NewRecursiveUnionExpression(
		expressions.ForEachQuantifier(initialRef),
		expressions.ForEachQuantifier(recursiveRef),
		scanAlias,
		insertAlias,
		expressions.TraversalLevel,
	)
	topRef := expressions.InitialOf(recUnion)

	// Implement inners so lack of physical plan is not the blocker.
	FireExpressionRule(NewPrimaryScanRule(), initialScanRef)
	FireExpressionRule(NewImplementTempTableInsertRule(), initialRef)
	FireExpressionRule(NewPrimaryScanRule(), recursiveScanRef)
	FireExpressionRule(NewImplementTempTableInsertRule(), recursiveRef)

	yielded := FireExpressionRule(NewImplementRecursiveDfsJoinRule(), topRef)
	if len(yielded) != 0 {
		t.Fatalf("ImplementRecursiveDfsJoinRule should NOT fire for TraversalLevel; yielded %d", len(yielded))
	}
}

func TestImplementRecursiveDfsJoin_NoFireWithoutPhysicalInner(t *testing.T) {
	t.Parallel()

	scanAlias := values.NamedCorrelationIdentifier("tt_scan_nf")
	insertAlias := values.NamedCorrelationIdentifier("tt_insert_nf")

	initialScan := expressions.NewFullUnorderedScanExpression([]string{"Seed"}, values.UnknownType)
	initialScanRef := expressions.InitialOf(initialScan)
	initialInsert := expressions.NewTempTableInsertExpression(
		expressions.ForEachQuantifier(initialScanRef),
		insertAlias,
		true,
	)
	initialRef := expressions.InitialOf(initialInsert)

	recursiveScan := expressions.NewFullUnorderedScanExpression([]string{"Step"}, values.UnknownType)
	recursiveScanRef := expressions.InitialOf(recursiveScan)
	recursiveInsert := expressions.NewTempTableInsertExpression(
		expressions.ForEachQuantifier(recursiveScanRef),
		insertAlias,
		false,
	)
	recursiveRef := expressions.InitialOf(recursiveInsert)

	recUnion := expressions.NewRecursiveUnionExpression(
		expressions.ForEachQuantifier(initialRef),
		expressions.ForEachQuantifier(recursiveRef),
		scanAlias,
		insertAlias,
		expressions.TraversalAny,
	)
	topRef := expressions.InitialOf(recUnion)

	// Do NOT implement inner plans — rule should not fire.
	yielded := FireExpressionRule(NewImplementRecursiveDfsJoinRule(), topRef)
	if len(yielded) != 0 {
		t.Fatalf("ImplementRecursiveDfsJoinRule fired without physical inners; yielded %d", len(yielded))
	}
}

func TestImplementRecursiveDfsJoin_ViaPlanner(t *testing.T) {
	t.Parallel()

	scanAlias := values.NamedCorrelationIdentifier("tt_scan_plan")
	insertAlias := values.NamedCorrelationIdentifier("tt_insert_plan")

	initialScan := expressions.NewFullUnorderedScanExpression([]string{"Seed"}, values.UnknownType)
	initialScanRef := expressions.InitialOf(initialScan)
	initialInsert := expressions.NewTempTableInsertExpression(
		expressions.ForEachQuantifier(initialScanRef),
		insertAlias,
		true,
	)
	initialRef := expressions.InitialOf(initialInsert)

	recursiveScan := expressions.NewFullUnorderedScanExpression([]string{"Step"}, values.UnknownType)
	recursiveScanRef := expressions.InitialOf(recursiveScan)
	recursiveInsert := expressions.NewTempTableInsertExpression(
		expressions.ForEachQuantifier(recursiveScanRef),
		insertAlias,
		false,
	)
	recursiveRef := expressions.InitialOf(recursiveInsert)

	recUnion := expressions.NewRecursiveUnionExpression(
		expressions.ForEachQuantifier(initialRef),
		expressions.ForEachQuantifier(recursiveRef),
		scanAlias,
		insertAlias,
		expressions.TraversalAny,
	)
	topRef := expressions.InitialOf(recUnion)

	rules := DefaultExpressionRules()
	p := NewPlanner(rules, EmptyPlanContext()).
		WithPlanningExpressionRules(BatchAExpressionRules()).
		WithImplementationRules(DefaultImplementationRules())
	if _, _, err := p.Plan(topRef); err != nil {
		t.Fatalf("Plan: %v", err)
	}

	foundDfs := false
	for _, m := range topRef.AllMembers() {
		if IsPhysicalRecursiveDfsJoin(m) {
			foundDfs = true
			break
		}
	}
	if !foundDfs {
		t.Fatal("planner did not produce a physical RecursiveDfsJoin member")
	}
}

func TestImplementRecursiveDfsJoin_PlanOutput(t *testing.T) {
	t.Parallel()

	scanAlias := values.NamedCorrelationIdentifier("tt_scan_out")
	insertAlias := values.NamedCorrelationIdentifier("tt_insert_out")

	initialScan := expressions.NewFullUnorderedScanExpression([]string{"Seed"}, values.UnknownType)
	initialScanRef := expressions.InitialOf(initialScan)
	initialInsert := expressions.NewTempTableInsertExpression(
		expressions.ForEachQuantifier(initialScanRef),
		insertAlias,
		true,
	)
	initialRef := expressions.InitialOf(initialInsert)

	recursiveScan := expressions.NewFullUnorderedScanExpression([]string{"Step"}, values.UnknownType)
	recursiveScanRef := expressions.InitialOf(recursiveScan)
	recursiveInsert := expressions.NewTempTableInsertExpression(
		expressions.ForEachQuantifier(recursiveScanRef),
		insertAlias,
		false,
	)
	recursiveRef := expressions.InitialOf(recursiveInsert)

	recUnion := expressions.NewRecursiveUnionExpression(
		expressions.ForEachQuantifier(initialRef),
		expressions.ForEachQuantifier(recursiveRef),
		scanAlias,
		insertAlias,
		expressions.TraversalAny,
	)
	topRef := expressions.InitialOf(recUnion)

	rules := DefaultExpressionRules()
	p := NewPlanner(rules, EmptyPlanContext()).
		WithPlanningExpressionRules(BatchAExpressionRules()).
		WithImplementationRules(DefaultImplementationRules())
	if _, _, err := p.Plan(topRef); err != nil {
		t.Fatalf("Plan: %v", err)
	}

	// Find the physical RecursiveDfsJoin among the members and verify
	// it carries a plan with an Explain string.
	var dfsWrap *plans.RecordQueryRecursiveDfsJoinPlan
	for _, m := range topRef.AllMembers() {
		if w, ok := m.(*plans.RecordQueryRecursiveDfsJoinPlan); ok {
			dfsWrap = w
			break
		}
	}
	if dfsWrap == nil {
		t.Fatal("no plans.RecordQueryRecursiveDfsJoinPlan found in topRef members")
	}

	rqp := dfsWrap.GetRecordQueryPlan()
	if rqp == nil {
		t.Fatal("GetRecordQueryPlan() returned nil")
	}
	explain := ExplainPhysicalPlan(dfsWrap)
	if explain == "" {
		t.Fatal("ExplainPhysicalPlan returned empty")
	}
	t.Logf("RecursiveDfsJoin Explain: %s", explain)
}

// --- ImplementRecursiveLevelUnionRule ---

// buildLevelUnionTree is a helper that builds a RecursiveUnionExpression
// tree with the given strategy and returns the top-level reference plus
// the inner references needed for pre-implementing inner plans.
func buildLevelUnionTree(
	strategy expressions.TraversalStrategy,
) (topRef *expressions.Reference, initialScanRef, initialRef, recursiveScanRef, recursiveRef *expressions.Reference) {
	scanAlias := values.UniqueCorrelationIdentifier()
	insertAlias := values.UniqueCorrelationIdentifier()

	initScan := expressions.NewFullUnorderedScanExpression([]string{"Seed"}, values.UnknownType)
	initialScanRef = expressions.InitialOf(initScan)
	initInsert := expressions.NewTempTableInsertExpression(
		expressions.ForEachQuantifier(initialScanRef),
		insertAlias, true,
	)
	initialRef = expressions.InitialOf(initInsert)

	recScan := expressions.NewFullUnorderedScanExpression([]string{"Step"}, values.UnknownType)
	recursiveScanRef = expressions.InitialOf(recScan)
	recInsert := expressions.NewTempTableInsertExpression(
		expressions.ForEachQuantifier(recursiveScanRef),
		insertAlias, false,
	)
	recursiveRef = expressions.InitialOf(recInsert)

	recUnion := expressions.NewRecursiveUnionExpression(
		expressions.ForEachQuantifier(initialRef),
		expressions.ForEachQuantifier(recursiveRef),
		scanAlias, insertAlias, strategy,
	)
	topRef = expressions.InitialOf(recUnion)
	return
}

func implementInnerCTEPlans(initialScanRef, initialRef, recursiveScanRef, recursiveRef *expressions.Reference) {
	FireExpressionRule(NewPrimaryScanRule(), initialScanRef)
	FireExpressionRule(NewImplementTempTableInsertRule(), initialRef)
	FireExpressionRule(NewPrimaryScanRule(), recursiveScanRef)
	FireExpressionRule(NewImplementTempTableInsertRule(), recursiveRef)
}

func TestImplementRecursiveLevelUnion_TraversalLevel_Fires(t *testing.T) {
	t.Parallel()

	topRef, isr, ir, rsr, rr := buildLevelUnionTree(expressions.TraversalLevel)
	implementInnerCTEPlans(isr, ir, rsr, rr)

	yielded := FireExpressionRule(NewImplementRecursiveLevelUnionRule(), topRef)
	if len(yielded) != 1 {
		t.Fatalf("ImplementRecursiveLevelUnionRule yielded %d, want 1", len(yielded))
	}

	if !IsPhysicalRecursiveLevelUnion(yielded[0]) {
		t.Fatalf("yield = %T, want *plans.RecordQueryRecursiveLevelUnionPlan", yielded[0])
	}

	wrap := yielded[0].(*plans.RecordQueryRecursiveLevelUnionPlan)
	plan, ok := wrap.GetRecordQueryPlan().(*plans.RecordQueryRecursiveLevelUnionPlan)
	if !ok {
		t.Fatalf("GetRecordQueryPlan() = %T, want *RecordQueryRecursiveLevelUnionPlan", wrap.GetRecordQueryPlan())
	}
	if plan.GetInitialState() == nil || plan.GetRecursiveState() == nil {
		t.Fatal("plan legs should not be nil")
	}
}

func TestImplementRecursiveLevelUnion_TraversalAny_Fires(t *testing.T) {
	t.Parallel()

	topRef, isr, ir, rsr, rr := buildLevelUnionTree(expressions.TraversalAny)
	implementInnerCTEPlans(isr, ir, rsr, rr)

	yielded := FireExpressionRule(NewImplementRecursiveLevelUnionRule(), topRef)
	if len(yielded) != 1 {
		t.Fatalf("ImplementRecursiveLevelUnionRule yielded %d for TraversalAny, want 1", len(yielded))
	}
	if !IsPhysicalRecursiveLevelUnion(yielded[0]) {
		t.Fatalf("yield = %T, want *plans.RecordQueryRecursiveLevelUnionPlan", yielded[0])
	}
}

func TestImplementRecursiveLevelUnion_TraversalPreorder_DoesNotFire(t *testing.T) {
	t.Parallel()

	topRef, isr, ir, rsr, rr := buildLevelUnionTree(expressions.TraversalPreorder)
	implementInnerCTEPlans(isr, ir, rsr, rr)

	yielded := FireExpressionRule(NewImplementRecursiveLevelUnionRule(), topRef)
	if len(yielded) != 0 {
		t.Fatalf("ImplementRecursiveLevelUnionRule should NOT fire for TraversalPreorder; yielded %d", len(yielded))
	}
}

func TestImplementRecursiveLevelUnion_TraversalPostorder_DoesNotFire(t *testing.T) {
	t.Parallel()

	topRef, isr, ir, rsr, rr := buildLevelUnionTree(expressions.TraversalPostorder)
	implementInnerCTEPlans(isr, ir, rsr, rr)

	yielded := FireExpressionRule(NewImplementRecursiveLevelUnionRule(), topRef)
	if len(yielded) != 0 {
		t.Fatalf("ImplementRecursiveLevelUnionRule should NOT fire for TraversalPostorder; yielded %d", len(yielded))
	}
}

func TestImplementRecursiveLevelUnion_NoFireWithoutPhysicalInner(t *testing.T) {
	t.Parallel()

	topRef, _, _, _, _ := buildLevelUnionTree(expressions.TraversalLevel)
	// Do NOT implement inner plans.
	yielded := FireExpressionRule(NewImplementRecursiveLevelUnionRule(), topRef)
	if len(yielded) != 0 {
		t.Fatalf("ImplementRecursiveLevelUnionRule fired without physical inners; yielded %d", len(yielded))
	}
}

func TestImplementRecursiveLevelUnion_ViaPlanner(t *testing.T) {
	t.Parallel()

	topRef, _, _, _, _ := buildLevelUnionTree(expressions.TraversalLevel)

	rules := DefaultExpressionRules()
	p := NewPlanner(rules, EmptyPlanContext()).
		WithPlanningExpressionRules(BatchAExpressionRules()).
		WithImplementationRules(DefaultImplementationRules())
	if _, _, err := p.Plan(topRef); err != nil {
		t.Fatalf("Plan: %v", err)
	}

	foundLevel := false
	for _, m := range topRef.AllMembers() {
		if IsPhysicalRecursiveLevelUnion(m) {
			foundLevel = true
			break
		}
	}
	if !foundLevel {
		t.Fatal("planner did not produce a physical RecursiveLevelUnion member")
	}
}

func TestImplementRecursiveLevelUnion_PlanOutput(t *testing.T) {
	t.Parallel()

	topRef, _, _, _, _ := buildLevelUnionTree(expressions.TraversalLevel)

	rules := DefaultExpressionRules()
	p := NewPlanner(rules, EmptyPlanContext()).
		WithPlanningExpressionRules(BatchAExpressionRules()).
		WithImplementationRules(DefaultImplementationRules())
	if _, _, err := p.Plan(topRef); err != nil {
		t.Fatalf("Plan: %v", err)
	}

	var wrap *plans.RecordQueryRecursiveLevelUnionPlan
	for _, m := range topRef.AllMembers() {
		if w, ok := m.(*plans.RecordQueryRecursiveLevelUnionPlan); ok {
			wrap = w
			break
		}
	}
	if wrap == nil {
		t.Fatal("no plans.RecordQueryRecursiveLevelUnionPlan found")
	}

	rqp := wrap.GetRecordQueryPlan()
	if rqp == nil {
		t.Fatal("GetRecordQueryPlan() returned nil")
	}
	explain := ExplainPhysicalPlan(wrap)
	if explain == "" {
		t.Fatal("ExplainPhysicalPlan returned empty")
	}
	t.Logf("RecursiveLevelUnion Explain: %s", explain)
}

func TestImplementRecursiveLevelUnion_BothRulesFire_TraversalAny(t *testing.T) {
	t.Parallel()

	// FORMATION (both alternatives): fire the two implementation rules
	// directly on a prepared TraversalAny tree — BOTH must yield. The
	// full-planner run below can no longer show both alternatives at
	// once: physical yields land ONLY in FinalMembers and OptimizeGroup
	// prunes finals to the winner (Java's prune-to-1), so the losing
	// alternative is not visible in AllMembers() after Plan().
	topRef, isr, ir, rsr, rr := buildLevelUnionTree(expressions.TraversalAny)
	implementInnerCTEPlans(isr, ir, rsr, rr)
	levelYields := FireExpressionRule(NewImplementRecursiveLevelUnionRule(), topRef)
	if len(levelYields) != 1 || !IsPhysicalRecursiveLevelUnion(levelYields[0]) {
		t.Fatalf("TraversalAny: ImplementRecursiveLevelUnionRule must yield the LevelUnion alternative; got %d yields", len(levelYields))
	}
	dfsYields := FireExpressionRule(NewImplementRecursiveDfsJoinRule(), topRef)
	if len(dfsYields) != 1 || !IsPhysicalRecursiveDfsJoin(dfsYields[0]) {
		t.Fatalf("TraversalAny: ImplementRecursiveDfsJoinRule must yield the DFS alternative; got %d yields", len(dfsYields))
	}

	// WINNER: the full planner keeps exactly one of the two alternatives.
	planRef, _, _, _, _ := buildLevelUnionTree(expressions.TraversalAny)

	rules := DefaultExpressionRules()
	p := NewPlanner(rules, EmptyPlanContext()).
		WithPlanningExpressionRules(BatchAExpressionRules()).
		WithImplementationRules(DefaultImplementationRules())
	if _, _, err := p.Plan(planRef); err != nil {
		t.Fatalf("Plan: %v", err)
	}

	foundImpl := false
	for _, m := range planRef.AllMembers() {
		if IsPhysicalRecursiveDfsJoin(m) || IsPhysicalRecursiveLevelUnion(m) {
			foundImpl = true
			break
		}
	}
	if !foundImpl {
		t.Fatal("TraversalAny should keep a physical recursive-union winner (DFS join or LevelUnion), found neither")
	}
}
