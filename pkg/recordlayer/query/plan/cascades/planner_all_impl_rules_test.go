package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

func TestAllImplRules_DefaultListHas7Rules(t *testing.T) {
	t.Parallel()
	rules := DefaultImplementationRules()
	// 11 constraint-push + 9 Java-ported + 1 DistinctFinal + 1 Go extension (ImplementInMemorySortRule)
	if len(rules) != 22 {
		t.Fatalf("expected 22 implementation rules, got %d", len(rules))
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
	unionRef := expressions.NewFinalReference([]expressions.RelationalExpression{union})

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

	finals := rootRef.FinalMembers()
	if len(finals) == 0 {
		t.Fatal("root should have final members after direct rule firing")
	}
}

func TestAllImplRules_SelectNoPredicatesPassThrough(t *testing.T) {
	t.Parallel()

	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	sw := &physicalScanWrapper{plan: scan}
	innerRef := expressions.NewFinalReference([]expressions.RelationalExpression{sw})
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

	finals := rootRef.FinalMembers()
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
	refA := expressions.NewFinalReference([]expressions.RelationalExpression{swA})
	pmA := NewPlanPropertiesMap()
	pmA.Add(swA)
	refA.SetPlanProperties(pmA)

	scanB := plans.NewRecordQueryScanPlan([]string{"B"}, values.UnknownType, false)
	swB := &physicalScanWrapper{plan: scanB}
	refB := expressions.NewFinalReference([]expressions.RelationalExpression{swB})
	pmB := NewPlanPropertiesMap()
	pmB.Add(swB)
	refB.SetPlanProperties(pmB)

	scanC := plans.NewRecordQueryScanPlan([]string{"C"}, values.UnknownType, false)
	swC := &physicalScanWrapper{plan: scanC}
	refC := expressions.NewFinalReference([]expressions.RelationalExpression{swC})
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

	finals := rootRef.FinalMembers()
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
