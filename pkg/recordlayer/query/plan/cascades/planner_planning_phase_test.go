package cascades

import (
	"fmt"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// planWithImplRules runs the full planner pipeline: REWRITING (Explore)
// + PLANNING (implementation rules) on the given root Reference.
// Returns the planner for further inspection (Members, properties).
func planWithImplRules(t *testing.T, rootRef *expressions.Reference, implRules []ImplementationRule) *Planner {
	t.Helper()
	p := NewPlanner(allRules(), nil).
		WithPlanningExpressionRules(BatchAExpressionRules()).
		WithImplementationRules(implRules)
	_, _, err := p.Plan(rootRef)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	return p
}

// ---------------------------------------------------------------------------
// 1. UniqueOverScan: Unique is absorbed because scans are always distinct.
// ---------------------------------------------------------------------------

func TestPlanner_PlanningPhase_UniqueOverScan(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	unique := expressions.NewLogicalUniqueExpression(
		expressions.ForEachQuantifier(scanRef),
	)
	rootRef := expressions.InitialOf(unique)

	planWithImplRules(t, rootRef, DefaultImplementationRules())

	// ImplementUniqueRule fires during the PLANNING phase. Because the
	// inner scan is always distinct, the Unique operator is absorbed:
	// the root Reference's members should contain the inner scan
	// wrapper directly (promoted from the inner ref), NOT a Unique
	// wrapper around it.
	finals := rootRef.AllMembers()
	if len(finals) == 0 {
		t.Fatal("root Reference has no members — ImplementUniqueRule did not fire")
	}

	foundScan := false
	for _, f := range finals {
		// RFC-184 W2: a bare primary scan is its own physical expression.
		if _, ok := f.(*plans.RecordQueryScanPlan); ok {
			foundScan = true
			break
		}
	}
	if !foundScan {
		types := make([]string, len(finals))
		for i, f := range finals {
			types[i] = fmt.Sprintf("%T", f)
		}
		t.Fatalf("expected *plans.RecordQueryScanPlan in members (Unique absorbed), got types: %v", types)
	}
}

// ---------------------------------------------------------------------------
// 2. UnorderedUnion over two scans.
// ---------------------------------------------------------------------------

func TestPlanner_PlanningPhase_UnorderedUnionOverTwoScans(t *testing.T) {
	t.Parallel()

	// FORMATION: ImplementUnorderedUnionRule itself must yield the
	// unordered wrapper over two physical scans. The full-planner run
	// below can no longer pin per-implementation visibility — physical
	// yields land ONLY in FinalMembers and OptimizeGroup prunes finals to
	// the winner (Java's prune-to-1), so the unordered wrapper may lose
	// to the sibling ordered-union implementation and vanish from
	// AllMembers(). Fire the rule directly to pin that it FORMS.
	{
		wA := plans.NewRecordQueryScanPlan([]string{"A"}, values.UnknownType, false)
		wB := plans.NewRecordQueryScanPlan([]string{"B"}, values.UnknownType, false)
		refA := expressions.InitialOf(wA)
		pmA := NewPlanPropertiesMap()
		pmA.Add(wA)
		refA.SetPlanProperties(pmA)
		refB := expressions.InitialOf(wB)
		pmB := NewPlanPropertiesMap()
		pmB.Add(wB)
		refB.SetPlanProperties(pmB)
		outerRef := expressions.InitialOf(expressions.NewLogicalUnionExpression([]expressions.Quantifier{
			expressions.ForEachQuantifier(refA),
			expressions.ForEachQuantifier(refB),
		}))

		var formed *plans.RecordQueryUnorderedUnionPlan
		for _, y := range FireImplementationRule(NewImplementUnorderedUnionRule(), outerRef) {
			if w, ok := y.(*plans.RecordQueryUnorderedUnionPlan); ok {
				formed = w
				break
			}
		}
		if formed == nil {
			t.Fatal("ImplementUnorderedUnionRule did not yield a *RecordQueryUnorderedUnionPlan")
		}
		uup := formed
		if got := len(uup.GetChildren()); got != 2 {
			t.Fatalf("formed unordered union children: got %d, want 2", got)
		}
	}

	// WINNER SHAPE: after the full planner run, SOME physical union
	// implementation (unordered or ordered — whichever the cost model
	// keeps) must survive in the root's members with a valid 2-child plan.
	scanA := expressions.NewFullUnorderedScanExpression([]string{"A"}, values.UnknownType)
	scanB := expressions.NewFullUnorderedScanExpression([]string{"B"}, values.UnknownType)
	union := expressions.NewLogicalUnionExpression([]expressions.Quantifier{
		expressions.ForEachQuantifier(expressions.InitialOf(scanA)),
		expressions.ForEachQuantifier(expressions.InitialOf(scanB)),
	})
	rootRef := expressions.InitialOf(union)

	planWithImplRules(t, rootRef, DefaultImplementationRules())

	finals := rootRef.AllMembers()
	if len(finals) == 0 {
		t.Fatal("root Reference has no members after PLANNING phase")
	}

	var unionPlan plans.RecordQueryPlan
	for _, f := range finals {
		switch w := f.(type) {
		case *plans.RecordQueryUnorderedUnionPlan:
			unionPlan = w.GetRecordQueryPlan()
		case *plans.RecordQueryUnionPlan:
			unionPlan = w.GetRecordQueryPlan()
		}
		if unionPlan != nil {
			break
		}
	}
	if unionPlan == nil {
		types := make([]string, len(finals))
		for i, f := range finals {
			types[i] = fmt.Sprintf("%T", f)
		}
		t.Fatalf("expected a physical union wrapper (unordered or ordered) in members, got types: %v", types)
	}
	kids, ok := unionPlan.(interface {
		GetChildren() []plans.RecordQueryPlan
	})
	if !ok {
		t.Fatalf("winning union plan %T has no children accessor", unionPlan)
	}
	if got := len(kids.GetChildren()); got != 2 {
		t.Fatalf("winning union children: got %d, want 2", got)
	}
}

// ---------------------------------------------------------------------------
// 3. SelectExpression with a predicate over a scan.
// ---------------------------------------------------------------------------

func TestPlanner_PlanningPhase_SelectWithPredicateOverScan(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)

	// Result value = QuantifiedObjectValue referencing the single
	// quantifier's alias, so isQuantifiedObjectValueFor returns true
	// and the rule yields a PredicatesFilter (not a Map).
	rv := values.NewQuantifiedObjectValue(q.GetAlias())

	// WHERE active = true — a ComparisonPredicate.
	pred := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "active", Typ: values.TypeBool},
		predicates.NewLiteralComparison(predicates.ComparisonEquals, true),
	)

	sel := expressions.NewSelectExpression(rv, []expressions.Quantifier{q},
		[]predicates.QueryPredicate{pred})
	rootRef := expressions.InitialOf(sel)

	// Explicitly include ImplementSimpleSelectRule (currently disabled in
	// DefaultImplementationRules) so we get a physical PredicatesFilter.
	// PrimaryScanRule is required so the inner scan gets a physical wrapper
	// before ImplementSimpleSelectRule fires (it needs a physical inner).
	implRules := []ImplementationRule{
		AsImplementationRule(NewPrimaryScanRule()),
		NewImplementSimpleSelectRule(),
		NewImplementUniqueRule(),
		NewImplementUnorderedUnionRule(),
	}

	planWithImplRules(t, rootRef, implRules)

	// ImplementSimpleSelectRule should yield a physicalPredicatesFilterWrapper
	// into the root Reference's members.
	finals := rootRef.AllMembers()
	if len(finals) == 0 {
		t.Fatal("root Reference has no members after PLANNING phase")
	}

	foundFilter := false
	for _, f := range finals {
		if _, ok := f.(*physicalPredicatesFilterWrapper); ok {
			foundFilter = true
			break
		}
	}
	if !foundFilter {
		types := make([]string, len(finals))
		for i, f := range finals {
			types[i] = fmt.Sprintf("%T", f)
		}
		t.Fatalf("expected *physicalPredicatesFilterWrapper in members, got types: %v", types)
	}
}

// ---------------------------------------------------------------------------
// 4. Physical scan wrapper in Members after PLANNING phase on a leaf.
// ---------------------------------------------------------------------------

func TestPlanner_PlanningPhase_FinalizeExpressions_LeafExpression(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"Leaf"}, values.UnknownType)
	rootRef := expressions.InitialOf(scan)

	planWithImplRules(t, rootRef, DefaultImplementationRules())

	// After PLANNING, PrimaryScanRule (via BatchAExpressionRules fired during
	// PLANNING) inserts a bare RecordQueryScanPlan into Members. Verify it is present.
	all := rootRef.AllMembers()
	if len(all) == 0 {
		t.Fatal("root Reference has no members for leaf expression")
	}

	// At least the original scan or a physical scan plan should be present.
	foundScan := false
	for _, f := range all {
		switch f.(type) {
		case *expressions.FullUnorderedScanExpression:
			foundScan = true
		case *plans.RecordQueryScanPlan:
			foundScan = true
		}
	}
	if !foundScan {
		t.Fatal("expected a scan expression in members")
	}
}

// ---------------------------------------------------------------------------
// 5. PlanProperties computed on References after PLANNING phase.
// ---------------------------------------------------------------------------

func TestPlanner_PlanningPhase_PropertiesComputedOnReference(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	unique := expressions.NewLogicalUniqueExpression(
		expressions.ForEachQuantifier(scanRef),
	)
	rootRef := expressions.InitialOf(unique)

	planWithImplRules(t, rootRef, DefaultImplementationRules())

	// After the PLANNING phase, computeRefPlanProperties runs on every
	// visited Reference. The inner scanRef must have PlanProperties set.
	pm := GetRefPlanPropertiesMap(scanRef)
	if pm == nil {
		t.Fatal("inner scanRef has nil PlanProperties after PLANNING phase")
	}

	// The scan wrapper should be in the properties map.
	exprs := pm.Expressions()
	if len(exprs) == 0 {
		t.Fatal("PlanPropertiesMap has no expressions — scan wrapper not registered")
	}

	// Verify that properties for each expression are non-nil.
	for _, expr := range exprs {
		props := pm.GetProperties(expr)
		if props == nil {
			t.Fatalf("GetProperties returned nil for expression %T", expr)
		}
	}

	// Verify distinct=true for the scan wrapper.
	for _, expr := range exprs {
		if _, ok := expr.(*plans.RecordQueryScanPlan); ok {
			props := pm.GetProperties(expr)
			if !props.GetBool(properties.PropDistinctRecords) {
				t.Fatal("scan wrapper should have distinct=true")
			}
		}
	}

	// The root Reference should also have PlanProperties.
	rootPM := GetRefPlanPropertiesMap(rootRef)
	if rootPM == nil {
		t.Fatal("root Reference has nil PlanProperties after PLANNING phase")
	}
}

// ---------------------------------------------------------------------------
// 6. Planner without implementation rules skips PLANNING phase.
// ---------------------------------------------------------------------------

func TestPlanner_PlanningPhase_AlwaysRuns(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	rootRef := expressions.InitialOf(
		expressions.NewLogicalUniqueExpression(
			expressions.ForEachQuantifier(scanRef),
		),
	)

	// No WithImplementationRules — PLANNING still runs (data access,
	// plan properties, etc. are always computed).
	p := NewPlanner(allRules(), nil)
	_, _, err := p.Plan(rootRef)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	// PLANNING always computes plan properties on leaf References.
	pm := GetRefPlanPropertiesMap(scanRef)
	if pm == nil {
		t.Fatal("PlanProperties should be set — PLANNING always runs")
	}
}

// ---------------------------------------------------------------------------
// 7. Members populated after PLANNING phase.
// ---------------------------------------------------------------------------

func TestPlanner_PlanningPhase_MembersPopulated(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	unique := expressions.NewLogicalUniqueExpression(
		expressions.ForEachQuantifier(scanRef),
	)
	rootRef := expressions.InitialOf(unique)

	planWithImplRules(t, rootRef, DefaultImplementationRules())

	// After PLANNING, the root Reference should have members inserted by
	// implementation rules (physical wrappers go into Members).
	all := rootRef.AllMembers()
	if len(all) == 0 {
		t.Fatal("root Reference has no members after PLANNING phase")
	}

	// Verify at least one physical expression is present in the root.
	foundPhysical := false
	for _, m := range all {
		if _, ok := m.(physicalPlanExpression); ok {
			foundPhysical = true
			break
		}
	}
	if !foundPhysical {
		t.Fatal("root Reference has no physical members after PLANNING phase")
	}

	// The inner scanRef should also have physical members (from PrimaryScanRule).
	innerAll := scanRef.AllMembers()
	foundInnerPhysical := false
	for _, m := range innerAll {
		if _, ok := m.(physicalPlanExpression); ok {
			foundInnerPhysical = true
			break
		}
	}
	if !foundInnerPhysical {
		t.Fatal("inner scanRef has no physical members after PLANNING phase")
	}
}
