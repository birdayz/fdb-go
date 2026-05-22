package cascades

import (
	"fmt"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/properties"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// planWithImplRules runs the full planner pipeline: REWRITING (Explore)
// + PLANNING (implementation rules) on the given root Reference.
// Returns the planner for further inspection (FinalMembers, properties).
func planWithImplRules(t *testing.T, rootRef *expressions.Reference, implRules []ImplementationRule) *Planner {
	t.Helper()
	p := NewPlanner(allRules(), nil).
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
	// the root Reference's final members should contain the inner scan
	// wrapper directly (promoted from the inner ref), NOT a Unique
	// wrapper around it.
	finals := rootRef.FinalMembers()
	if len(finals) == 0 {
		t.Fatal("root Reference has no final members — ImplementUniqueRule did not fire")
	}

	foundScan := false
	for _, f := range finals {
		if _, ok := f.(*physicalScanWrapper); ok {
			foundScan = true
			break
		}
	}
	if !foundScan {
		types := make([]string, len(finals))
		for i, f := range finals {
			types[i] = fmt.Sprintf("%T", f)
		}
		t.Fatalf("expected *physicalScanWrapper in final members (Unique absorbed), got types: %v", types)
	}
}

// ---------------------------------------------------------------------------
// 2. UnorderedUnion over two scans.
// ---------------------------------------------------------------------------

func TestPlanner_PlanningPhase_UnorderedUnionOverTwoScans(t *testing.T) {
	t.Parallel()

	scanA := expressions.NewFullUnorderedScanExpression([]string{"A"}, values.UnknownType)
	scanB := expressions.NewFullUnorderedScanExpression([]string{"B"}, values.UnknownType)
	union := expressions.NewLogicalUnionExpression([]expressions.Quantifier{
		expressions.ForEachQuantifier(expressions.InitialOf(scanA)),
		expressions.ForEachQuantifier(expressions.InitialOf(scanB)),
	})
	rootRef := expressions.InitialOf(union)

	planWithImplRules(t, rootRef, DefaultImplementationRules())

	// ImplementUnorderedUnionRule should yield a
	// physicalUnorderedUnionWrapper into the root Reference's final members.
	finals := rootRef.FinalMembers()
	if len(finals) == 0 {
		t.Fatal("root Reference has no final members after PLANNING phase")
	}

	var wrapper *physicalUnorderedUnionWrapper
	for _, f := range finals {
		if w, ok := f.(*physicalUnorderedUnionWrapper); ok {
			wrapper = w
			break
		}
	}
	if wrapper == nil {
		types := make([]string, len(finals))
		for i, f := range finals {
			types[i] = fmt.Sprintf("%T", f)
		}
		t.Fatalf("expected *physicalUnorderedUnionWrapper in final members, got types: %v", types)
	}

	// The underlying plan must be a RecordQueryUnorderedUnionPlan with 2 children.
	uup, ok := wrapper.GetRecordQueryPlan().(*plans.RecordQueryUnorderedUnionPlan)
	if !ok {
		t.Fatalf("underlying plan: expected *RecordQueryUnorderedUnionPlan, got %T",
			wrapper.GetRecordQueryPlan())
	}
	if got := len(uup.GetChildren()); got != 2 {
		t.Fatalf("unordered union children: got %d, want 2", got)
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
		NewFinalizeExpressionsRule(),
	}

	planWithImplRules(t, rootRef, implRules)

	// ImplementSimpleSelectRule should yield a physicalPredicatesFilterWrapper
	// into the root Reference's final members.
	finals := rootRef.FinalMembers()
	if len(finals) == 0 {
		t.Fatal("root Reference has no final members after PLANNING phase")
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
		t.Fatalf("expected *physicalPredicatesFilterWrapper in final members, got types: %v", types)
	}
}

// ---------------------------------------------------------------------------
// 4. FinalizeExpressionsRule on a leaf (FullUnorderedScan).
// ---------------------------------------------------------------------------

func TestPlanner_PlanningPhase_FinalizeExpressions_LeafExpression(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"Leaf"}, values.UnknownType)
	rootRef := expressions.InitialOf(scan)

	planWithImplRules(t, rootRef, DefaultImplementationRules())

	// FinalizeExpressionsRule is the catch-all. For a leaf expression
	// with no children, it yields the expression directly into final
	// members. With BatchA rules also active, PrimaryScanRule adds a
	// physicalScanWrapper as an exploratory member, and
	// FinalizeExpressionsRule finalizes both.
	finals := rootRef.FinalMembers()
	if len(finals) == 0 {
		t.Fatal("root Reference has no final members for leaf expression")
	}

	// At least the original scan should be finalized.
	foundScan := false
	for _, f := range finals {
		switch f.(type) {
		case *expressions.FullUnorderedScanExpression:
			foundScan = true
		case *physicalScanWrapper:
			foundScan = true
		}
	}
	if !foundScan {
		t.Fatal("expected a scan expression in final members")
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
		if _, ok := expr.(*physicalScanWrapper); ok {
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

func TestPlanner_PlanningPhase_SkippedWhenNoImplRules(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	rootRef := expressions.InitialOf(
		expressions.NewLogicalUniqueExpression(
			expressions.ForEachQuantifier(scanRef),
		),
	)

	// No WithImplementationRules — PLANNING phase is skipped.
	p := NewPlanner(allRules(), nil)
	_, _, err := p.Plan(rootRef)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	// PlanProperties should NOT be set since PLANNING phase was skipped.
	pm := GetRefPlanPropertiesMap(scanRef)
	if pm != nil {
		t.Fatal("PlanProperties should be nil when PLANNING phase is skipped")
	}
}

// ---------------------------------------------------------------------------
// 7. FinalMembers populated after PLANNING phase.
// ---------------------------------------------------------------------------

func TestPlanner_PlanningPhase_FinalMembersPopulated(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	unique := expressions.NewLogicalUniqueExpression(
		expressions.ForEachQuantifier(scanRef),
	)
	rootRef := expressions.InitialOf(unique)

	planWithImplRules(t, rootRef, DefaultImplementationRules())

	// After PLANNING, the root Reference should have final members
	// inserted by the implementation rules.
	finals := rootRef.FinalMembers()
	if len(finals) == 0 {
		t.Fatal("root Reference has no final members after PLANNING phase")
	}

	// The inner scanRef should also have final members (from
	// FinalizeExpressionsRule or PrimaryScanRule flowing through
	// finalization).
	innerFinals := scanRef.FinalMembers()
	if len(innerFinals) == 0 {
		t.Fatal("inner scanRef has no final members after PLANNING phase")
	}
}
