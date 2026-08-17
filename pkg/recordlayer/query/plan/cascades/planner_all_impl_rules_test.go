package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func mustAllImplConstruct[T any](value T, err error) T {
	if err != nil {
		panic("construct all-implementation-rules fixture: " + err.Error())
	}
	return value
}

func allImplRowType() values.Type {
	return values.NewRecordType("AllImplRow", false, []values.Field{
		{Name: "id", FieldType: values.NotNullLong, Ordinal: 0},
	})
}

func allImplScan(recordType string) *plans.RecordQueryScanPlan {
	return mustAllImplConstruct(plans.NewRecordQueryScanPlan(
		[]string{recordType}, allImplRowType(), false))
}

func allImplScanWithPK(recordType string) (*plans.RecordQueryScanPlan, *expressions.Reference) {
	root := mustAllImplConstruct(values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier("all_impl_pk"), allImplRowType()))
	pk := mustAllImplConstruct(values.ResolveOrdinalSeedField(root, 0))
	scan := allImplScan(recordType).
		WithKeyComponentTypes([]values.Type{values.NotNullLong}).
		WithPrimaryKey([]values.Value{pk})
	ref := expressions.InitialOf(scan)
	pm := NewPlanPropertiesMap()
	pm.Add(scan)
	ref.SetPlanProperties(pm)
	return scan, ref
}

func TestAllImplRules_DefaultListHas7Rules(t *testing.T) {
	t.Parallel()
	rules := DefaultImplementationRules()
	// 15 ordering-push + 4 referenced-fields-push + 9 Java-ported + 13 fetch-push-through
	// (the ordered merge-sort union joined the set-op-through-fetch
	// family — Java PlanningRuleSet.java:158's UnionOnValues arm)
	// + 1 vector limit-fold (SinkLimitIntoVectorScanRule, RFC-156 Phase B)
	// + 1 Go extension (ImplementInMemorySortRule) = 43
	// Rules yield into Members.
	if len(rules) != 43 {
		t.Fatalf("expected 43 implementation rules, got %d", len(rules))
	}
}

func TestAllImplRules_UniqueOverDistinctUnion_WithPK_DirectFire(t *testing.T) {
	t.Parallel()

	_, refA := allImplScanWithPK("T")
	_, refB := allImplScanWithPK("T")

	union := mustAllImplConstruct(expressions.NewLogicalUnionExpression([]expressions.Quantifier{
		expressions.ForEachQuantifier(refA),
		expressions.ForEachQuantifier(refB),
	}))
	unionRef := expressions.InitialOf(union)

	distinct := mustAllImplConstruct(expressions.NewLogicalDistinctExpression(
		expressions.ForEachQuantifier(unionRef)))
	distinctRef := expressions.InitialOf(distinct)

	unique := mustAllImplConstruct(expressions.NewLogicalUniqueExpression(
		expressions.ForEachQuantifier(distinctRef)))
	rootRef := expressions.InitialOf(unique)

	for _, rule := range DefaultImplementationRules() {
		mustFireImplementationRule(t, rule, unionRef)
	}
	computeRefPlanProperties(unionRef)

	for _, rule := range DefaultImplementationRules() {
		mustFireImplementationRule(t, rule, distinctRef)
	}
	computeRefPlanProperties(distinctRef)

	for _, rule := range DefaultImplementationRules() {
		mustFireImplementationRule(t, rule, rootRef)
	}

	finals := rootRef.AllMembers()
	if len(finals) == 0 {
		t.Fatal("root should have members after direct rule firing")
	}
}

func TestAllImplRules_SelectNoPredicatesPassThrough(t *testing.T) {
	t.Parallel()

	scan := allImplScan("T")
	sw := scan
	innerRef := expressions.InitialOf(sw)
	pm := NewPlanPropertiesMap()
	pm.Add(sw)
	innerRef.SetPlanProperties(pm)

	q := expressions.ForEachQuantifier(innerRef)
	result := mustAllImplConstruct(values.NewQuantifiedObjectValue(q.GetAlias(), allImplRowType()))
	sel := mustAllImplConstruct(expressions.NewSelectExpression(
		result,
		[]expressions.Quantifier{q},
		nil,
	))
	rootRef := expressions.InitialOf(sel)

	for _, rule := range DefaultImplementationRules() {
		mustFireImplementationRule(t, rule, rootRef)
	}

	finals := rootRef.AllMembers()
	foundScan := false
	for _, f := range finals {
		if _, ok := f.(*plans.RecordQueryScanPlan); ok {
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

	scanA := allImplScan("A")
	swA := scanA
	refA := expressions.InitialOf(swA)
	pmA := NewPlanPropertiesMap()
	pmA.Add(swA)
	refA.SetPlanProperties(pmA)

	scanB := allImplScan("B")
	swB := scanB
	refB := expressions.InitialOf(swB)
	pmB := NewPlanPropertiesMap()
	pmB.Add(swB)
	refB.SetPlanProperties(pmB)

	scanC := allImplScan("C")
	swC := scanC
	refC := expressions.InitialOf(swC)
	pmC := NewPlanPropertiesMap()
	pmC.Add(swC)
	refC.SetPlanProperties(pmC)

	union := mustAllImplConstruct(expressions.NewLogicalUnionExpression([]expressions.Quantifier{
		expressions.ForEachQuantifier(refA),
		expressions.ForEachQuantifier(refB),
		expressions.ForEachQuantifier(refC),
	}))
	rootRef := expressions.InitialOf(union)

	for _, rule := range DefaultImplementationRules() {
		mustFireImplementationRule(t, rule, rootRef)
	}

	finals := rootRef.AllMembers()
	foundUnorderedUnion := false
	for _, f := range finals {
		if _, ok := f.(*plans.RecordQueryUnorderedUnionPlan); ok {
			foundUnorderedUnion = true
			break
		}
	}
	if !foundUnorderedUnion {
		t.Fatal("3-leg union should produce *RecordQueryUnorderedUnionPlan")
	}
}
