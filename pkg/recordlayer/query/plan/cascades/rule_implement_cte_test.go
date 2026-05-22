package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
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

	wrap, ok := yielded[0].(*physicalTempTableScanWrapper)
	if !ok {
		t.Fatalf("yield = %T, want *physicalTempTableScanWrapper", yielded[0])
	}
	plan, ok := wrap.GetRecordQueryPlan().(*plans.RecordQueryTempTableScanPlan)
	if !ok {
		t.Fatalf("GetRecordQueryPlan() = %T, want *RecordQueryTempTableScanPlan", wrap.GetRecordQueryPlan())
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

	rules := append(DefaultExpressionRules(), BatchAExpressionRules()...)
	p := NewPlanner(rules, EmptyPlanContext()).WithImplementationRules(DefaultImplementationRules())
	if _, _, err := p.Plan(ref); err != nil {
		t.Fatalf("Plan: %v", err)
	}

	foundPhysical := false
	for _, m := range ref.AllMembers() {
		if _, ok := m.(*physicalTempTableScanWrapper); ok {
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

	rules := append(DefaultExpressionRules(), BatchAExpressionRules()...)
	p := NewPlanner(rules, EmptyPlanContext()).WithImplementationRules(DefaultImplementationRules())
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

	wrap, ok := yielded[0].(*physicalTempTableInsertWrapper)
	if !ok {
		t.Fatalf("yield = %T, want *physicalTempTableInsertWrapper", yielded[0])
	}
	plan, ok := wrap.GetRecordQueryPlan().(*plans.RecordQueryTempTableInsertPlan)
	if !ok {
		t.Fatalf("GetRecordQueryPlan() = %T, want *RecordQueryTempTableInsertPlan", wrap.GetRecordQueryPlan())
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

	rules := append(DefaultExpressionRules(), BatchAExpressionRules()...)
	p := NewPlanner(rules, EmptyPlanContext()).WithImplementationRules(DefaultImplementationRules())
	plan, _, err := p.Plan(topRef)
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
		t.Fatalf("yield = %T, want *physicalRecursiveDfsJoinWrapper", yielded[0])
	}

	wrap := yielded[0].(*physicalRecursiveDfsJoinWrapper)
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

	wrap := yielded[0].(*physicalRecursiveDfsJoinWrapper)
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

	rules := append(DefaultExpressionRules(), BatchAExpressionRules()...)
	p := NewPlanner(rules, EmptyPlanContext()).WithImplementationRules(DefaultImplementationRules())
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

	rules := append(DefaultExpressionRules(), BatchAExpressionRules()...)
	p := NewPlanner(rules, EmptyPlanContext()).WithImplementationRules(DefaultImplementationRules())
	if _, _, err := p.Plan(topRef); err != nil {
		t.Fatalf("Plan: %v", err)
	}

	// Find the physical RecursiveDfsJoin among the members and verify
	// it carries a plan with an Explain string.
	var dfsWrap *physicalRecursiveDfsJoinWrapper
	for _, m := range topRef.AllMembers() {
		if w, ok := m.(*physicalRecursiveDfsJoinWrapper); ok {
			dfsWrap = w
			break
		}
	}
	if dfsWrap == nil {
		t.Fatal("no physicalRecursiveDfsJoinWrapper found in topRef members")
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
		t.Fatalf("yield = %T, want *physicalRecursiveLevelUnionWrapper", yielded[0])
	}

	wrap := yielded[0].(*physicalRecursiveLevelUnionWrapper)
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
		t.Fatalf("yield = %T, want *physicalRecursiveLevelUnionWrapper", yielded[0])
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

	rules := append(DefaultExpressionRules(), BatchAExpressionRules()...)
	p := NewPlanner(rules, EmptyPlanContext()).WithImplementationRules(DefaultImplementationRules())
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

	rules := append(DefaultExpressionRules(), BatchAExpressionRules()...)
	p := NewPlanner(rules, EmptyPlanContext()).WithImplementationRules(DefaultImplementationRules())
	if _, _, err := p.Plan(topRef); err != nil {
		t.Fatalf("Plan: %v", err)
	}

	var wrap *physicalRecursiveLevelUnionWrapper
	for _, m := range topRef.AllMembers() {
		if w, ok := m.(*physicalRecursiveLevelUnionWrapper); ok {
			wrap = w
			break
		}
	}
	if wrap == nil {
		t.Fatal("no physicalRecursiveLevelUnionWrapper found")
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

	topRef, _, _, _, _ := buildLevelUnionTree(expressions.TraversalAny)

	rules := append(DefaultExpressionRules(), BatchAExpressionRules()...)
	p := NewPlanner(rules, EmptyPlanContext()).WithImplementationRules(DefaultImplementationRules())
	if _, _, err := p.Plan(topRef); err != nil {
		t.Fatalf("Plan: %v", err)
	}

	foundDfs := false
	foundLevel := false
	for _, m := range topRef.AllMembers() {
		if IsPhysicalRecursiveDfsJoin(m) {
			foundDfs = true
		}
		if IsPhysicalRecursiveLevelUnion(m) {
			foundLevel = true
		}
	}
	if !foundDfs {
		t.Fatal("TraversalAny should produce a DFS alternative")
	}
	if !foundLevel {
		t.Fatal("TraversalAny should produce a LevelUnion alternative")
	}
}
