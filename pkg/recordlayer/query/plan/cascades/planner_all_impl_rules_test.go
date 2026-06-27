package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func TestAllImplRules_DefaultListHas7Rules(t *testing.T) {
	t.Parallel()
	rules := DefaultImplementationRules()
	// 15 ordering-push + 4 referenced-fields-push + 9 Java-ported + 12 fetch-push-through
	// + 1 Go extension (ImplementInMemorySortRule) = 41
	// Rules yield into Members.
	if len(rules) != 41 {
		t.Fatalf("expected 41 implementation rules, got %d", len(rules))
	}
}

func TestAllImplRules_UniqueOverDistinctUnion_WithPK_DirectFire(t *testing.T) {
	t.Parallel()

	_, refA := makeScanWithPK("T", "id")
	_, refB := makeScanWithPK("T", "id")

	union := expressions.NewLogicalUnionExpression([]expressions.Quantifier{
		expressions.ForEachQuantifier(refA),
		expressions.ForEachQuantifier(refB),
	})
	unionRef := expressions.InitialOf(union)

	distinct := expressions.NewLogicalDistinctExpression(
		expressions.ForEachQuantifier(unionRef))
	distinctRef := expressions.InitialOf(distinct)

	unique := expressions.NewLogicalUniqueExpression(
		expressions.ForEachQuantifier(distinctRef))
	rootRef := expressions.InitialOf(unique)

	for _, rule := range DefaultImplementationRules() {
		FireImplementationRule(rule, unionRef)
	}
	computeRefPlanProperties(unionRef)

	for _, rule := range DefaultImplementationRules() {
		FireImplementationRule(rule, distinctRef)
	}
	computeRefPlanProperties(distinctRef)

	for _, rule := range DefaultImplementationRules() {
		FireImplementationRule(rule, rootRef)
	}

	finals := rootRef.AllMembers()
	if len(finals) == 0 {
		t.Fatal("root should have members after direct rule firing")
	}
}

func TestAllImplRules_SelectNoPredicatesPassThrough(t *testing.T) {
	t.Parallel()

	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	sw := &physicalScanWrapper{plan: scan}
	innerRef := expressions.InitialOf(sw)
	pm := NewPlanPropertiesMap()
	pm.Add(sw)
	innerRef.SetPlanProperties(pm)

	q := expressions.ForEachQuantifier(innerRef)
	sel := expressions.NewSelectExpression(
		values.NewQuantifiedObjectValue(q.GetAlias()),
		[]expressions.Quantifier{q},
		nil,
	)
	rootRef := expressions.InitialOf(sel)

	for _, rule := range DefaultImplementationRules() {
		FireImplementationRule(rule, rootRef)
	}

	finals := rootRef.AllMembers()
	foundScan := false
	for _, f := range finals {
		if _, ok := f.(*physicalScanWrapper); ok {
			foundScan = true
			break
		}
	}
	if !foundScan {
		t.Fatal("SELECT with no predicates + simple result should pass through to scan")
	}
}

func TestAllImplRules_UnorderedUnionThreeLegs(t *testing.T) {
	t.Parallel()

	scanA := plans.NewRecordQueryScanPlan([]string{"A"}, values.UnknownType, false)
	swA := &physicalScanWrapper{plan: scanA}
	refA := expressions.InitialOf(swA)
	pmA := NewPlanPropertiesMap()
	pmA.Add(swA)
	refA.SetPlanProperties(pmA)

	scanB := plans.NewRecordQueryScanPlan([]string{"B"}, values.UnknownType, false)
	swB := &physicalScanWrapper{plan: scanB}
	refB := expressions.InitialOf(swB)
	pmB := NewPlanPropertiesMap()
	pmB.Add(swB)
	refB.SetPlanProperties(pmB)

	scanC := plans.NewRecordQueryScanPlan([]string{"C"}, values.UnknownType, false)
	swC := &physicalScanWrapper{plan: scanC}
	refC := expressions.InitialOf(swC)
	pmC := NewPlanPropertiesMap()
	pmC.Add(swC)
	refC.SetPlanProperties(pmC)

	union := expressions.NewLogicalUnionExpression([]expressions.Quantifier{
		expressions.ForEachQuantifier(refA),
		expressions.ForEachQuantifier(refB),
		expressions.ForEachQuantifier(refC),
	})
	rootRef := expressions.InitialOf(union)

	for _, rule := range DefaultImplementationRules() {
		FireImplementationRule(rule, rootRef)
	}

	finals := rootRef.AllMembers()
	foundUnorderedUnion := false
	for _, f := range finals {
		if _, ok := f.(*physicalUnorderedUnionWrapper); ok {
			foundUnorderedUnion = true
			break
		}
	}
	if !foundUnorderedUnion {
		t.Fatal("3-leg union should produce physicalUnorderedUnionWrapper")
	}
}
