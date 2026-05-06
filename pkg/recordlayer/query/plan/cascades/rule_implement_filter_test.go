package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

func TestImplementFilterRule_FiresAfterScanImplemented(t *testing.T) {
	t.Parallel()
	// Build Filter(P, Scan). Run PrimaryScanRule to add a physical
	// wrapper to the inner Reference. Then ImplementFilterRule should
	// fire and yield a FilterPlan.
	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	innerRef := expressions.InitialOf(scan)
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pred},
		expressions.ForEachQuantifier(innerRef),
	)
	topRef := expressions.InitialOf(filter)

	// Step 1: Implement the scan.
	scanRule := NewPrimaryScanRule()
	FireExpressionRule(scanRule, innerRef)
	// innerRef should now have 2 members (logical scan + physical wrapper).
	if got := len(innerRef.Members()); got != 2 {
		t.Fatalf("after PrimaryScanRule, innerRef has %d members, want 2", got)
	}

	// Step 2: Fire ImplementFilterRule on the top Reference.
	filterRule := NewImplementFilterRule()
	yielded := FireExpressionRule(filterRule, topRef)
	if len(yielded) != 1 {
		t.Fatalf("ImplementFilterRule yielded %d, want 1", len(yielded))
	}
	wrap, ok := yielded[0].(*physicalFilterWrapper)
	if !ok {
		t.Fatalf("yield = %T, want *physicalFilterWrapper", yielded[0])
	}
	plan := wrap.GetPlan()
	if plan == nil {
		t.Fatal("wrapper has no plan")
	}
	if got := len(plan.GetPredicates()); got != 1 {
		t.Fatalf("filter plan predicates = %d, want 1", got)
	}
	innerPlan, ok := plan.GetInner().(*plans.RecordQueryScanPlan)
	if !ok {
		t.Fatalf("filter plan inner = %T, want *RecordQueryScanPlan", plan.GetInner())
	}
	if rts := innerPlan.GetRecordTypes(); len(rts) != 1 || rts[0] != "Order" {
		t.Fatalf("inner scan record types = %v", rts)
	}
}

func TestImplementFilterRule_NoFireWithoutPhysicalInner(t *testing.T) {
	t.Parallel()
	// Filter(P, Scan) WITHOUT firing PrimaryScanRule first — inner
	// has no physical wrapper, so ImplementFilterRule should skip.
	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pred},
		expressions.ForEachQuantifier(expressions.InitialOf(scan)),
	)
	topRef := expressions.InitialOf(filter)

	filterRule := NewImplementFilterRule()
	yielded := FireExpressionRule(filterRule, topRef)
	if len(yielded) != 0 {
		t.Fatalf("ImplementFilterRule fired without physical inner; yielded %d", len(yielded))
	}
}

// TestBatchA_CostExtraction_PicksPhysicalOverLogical pins that
// after Batch A rules fire and the OPTIMIZE phase runs, the
// planner's BestMember is the physical wrapper (not the logical
// expression). This validates the CostHinter wiring: physical
// wrappers' HintCost returns a discounted cost via
// physicalWrapperCostMultiplier=0.9, so cost extraction prefers
// them over the logical equivalent.
func TestBatchA_CostExtraction_PicksPhysicalOverLogical(t *testing.T) {
	t.Parallel()
	pred := predicates.NewValuePredicate(&values.FieldValue{Field: "active", Typ: values.TypeBool})
	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pred},
		expressions.ForEachQuantifier(expressions.InitialOf(scan)),
	)
	ref := expressions.InitialOf(filter)

	rules := []ExpressionRule{
		NewPrimaryScanRule(),
		NewImplementFilterRule(),
	}
	p := NewPlanner(rules, nil)
	if _, conv := p.Explore(ref); !conv {
		t.Fatal("planner did not converge")
	}

	// After Explore, OPTIMIZE picks the cheapest. With CostHinter
	// applying the physical-wrapper discount, the physical filter
	// wrapper should win over the logical filter.
	best := p.BestMember(ref)
	if best == nil {
		t.Fatal("BestMember returned nil")
	}
	if _, ok := best.(*physicalFilterWrapper); !ok {
		t.Fatalf("BestMember = %T, want *physicalFilterWrapper (cost-driven extraction should pick physical)", best)
	}
}

// TestImplementFilterRule_FiresOnFilterOverDistinct pins that Filter
// over Distinct can be physical-implemented WITHOUT push rules first.
func TestImplementFilterRule_FiresOnFilterOverDistinct(t *testing.T) {
	t.Parallel()
	pred := predicates.NewValuePredicate(&values.FieldValue{Field: "active", Typ: values.TypeBool})
	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)

	distinctInner := expressions.NewLogicalDistinctExpression(
		expressions.ForEachQuantifier(expressions.InitialOf(scan)),
	)
	distinctRef := expressions.InitialOf(distinctInner)
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pred},
		expressions.ForEachQuantifier(distinctRef),
	)
	topRef := expressions.InitialOf(filter)

	scanRef := distinctInner.GetQuantifiers()[0].GetRangesOver()
	FireExpressionRule(NewPrimaryScanRule(), scanRef)
	FireExpressionRule(NewImplementDistinctRule(), distinctRef)

	yielded := FireExpressionRule(NewImplementFilterRule(), topRef)
	if len(yielded) != 1 {
		t.Fatalf("ImplementFilterRule yielded %d, want 1 (Filter over physical Distinct)", len(yielded))
	}
	wrap, ok := yielded[0].(*physicalFilterWrapper)
	if !ok {
		t.Fatalf("yield = %T, want *physicalFilterWrapper", yielded[0])
	}
	innerPlan := wrap.GetPlan().GetInner()
	if _, ok := innerPlan.(*plans.RecordQueryDistinctPlan); !ok {
		t.Fatalf("filter inner plan = %T, want *RecordQueryDistinctPlan", innerPlan)
	}
}

// TestImplementFilterRule_FiresOverPhysicalIntersection is the
// symmetric companion to the Filter-over-Union case below.
// SQL pattern: SELECT ... FROM (A INTERSECT B) WHERE pred. The
// 7-wrapper symmetry fix lets Filter recognise physicalIntersection
// wrappers as physical inners.
func TestImplementFilterRule_FiresOverPhysicalIntersection(t *testing.T) {
	t.Parallel()
	scanA := expressions.NewFullUnorderedScanExpression([]string{"A"}, values.UnknownType)
	scanB := expressions.NewFullUnorderedScanExpression([]string{"B"}, values.UnknownType)
	refA := expressions.InitialOf(scanA)
	refB := expressions.InitialOf(scanB)
	intr := expressions.NewLogicalIntersectionExpression(
		[]expressions.Quantifier{
			expressions.ForEachQuantifier(refA),
			expressions.ForEachQuantifier(refB),
		},
		nil,
	)
	intrRef := expressions.InitialOf(intr)
	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pred},
		expressions.ForEachQuantifier(intrRef),
	)
	topRef := expressions.InitialOf(filter)

	FireExpressionRule(NewPrimaryScanRule(), refA)
	FireExpressionRule(NewPrimaryScanRule(), refB)
	FireExpressionRule(NewImplementIntersectionRule(), intrRef)

	yielded := FireExpressionRule(NewImplementFilterRule(), topRef)
	if len(yielded) != 1 {
		t.Fatalf("ImplementFilterRule yielded %d, want 1 (Filter over physical Intersection)", len(yielded))
	}
	wrap := yielded[0].(*physicalFilterWrapper)
	if _, ok := wrap.GetPlan().GetInner().(*plans.RecordQueryIntersectionPlan); !ok {
		t.Fatalf("inner = %T, want *RecordQueryIntersectionPlan", wrap.GetPlan().GetInner())
	}
}

// TestImplementFilterRule_FiresOverPhysicalUnion pins that
// ImplementFilterRule fires when the inner Reference contains a
// physicalUnionWrapper — closes the 7-wrapper symmetry gap caught
// by the reviewer's late-shift batch review.
//
// Filter(Union(Scan, Scan)) should physically implement to
// FilterPlan(UnionPlan(ScanPlan, ScanPlan)) once both scans + the
// union are physically implemented.
func TestImplementFilterRule_FiresOverPhysicalUnion(t *testing.T) {
	t.Parallel()
	scanA := expressions.NewFullUnorderedScanExpression([]string{"A"}, values.UnknownType)
	scanB := expressions.NewFullUnorderedScanExpression([]string{"B"}, values.UnknownType)
	refA := expressions.InitialOf(scanA)
	refB := expressions.InitialOf(scanB)
	union := expressions.NewLogicalUnionExpression([]expressions.Quantifier{
		expressions.ForEachQuantifier(refA),
		expressions.ForEachQuantifier(refB),
	})
	unionRef := expressions.InitialOf(union)
	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pred},
		expressions.ForEachQuantifier(unionRef),
	)
	topRef := expressions.InitialOf(filter)

	// Step 1: Implement both scans.
	FireExpressionRule(NewPrimaryScanRule(), refA)
	FireExpressionRule(NewPrimaryScanRule(), refB)
	// Step 2: Implement the union.
	FireExpressionRule(NewImplementUnionRule(), unionRef)
	// Step 3: Now Filter's inner Reference has a physicalUnionWrapper.
	yielded := FireExpressionRule(NewImplementFilterRule(), topRef)
	if len(yielded) != 1 {
		t.Fatalf("ImplementFilterRule yielded %d, want 1 (Filter over physical Union)", len(yielded))
	}
	wrap, ok := yielded[0].(*physicalFilterWrapper)
	if !ok {
		t.Fatalf("yield = %T, want *physicalFilterWrapper", yielded[0])
	}
	innerPlan := wrap.GetPlan().GetInner()
	if _, ok := innerPlan.(*plans.RecordQueryUnionPlan); !ok {
		t.Fatalf("filter inner plan = %T, want *RecordQueryUnionPlan", innerPlan)
	}
}

func TestPlannerWithBatchA_ImplementsFilterOverScan(t *testing.T) {
	t.Parallel()
	// End-to-end through the task-stack Planner: Filter(P, Scan) with
	// PrimaryScanRule + ImplementFilterRule in the rule set converges
	// to a Reference holding both the original logical Filter AND a
	// physical FilterPlan-over-ScanPlan member.
	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pred},
		expressions.ForEachQuantifier(expressions.InitialOf(scan)),
	)
	ref := expressions.InitialOf(filter)

	rules := []ExpressionRule{
		NewPrimaryScanRule(),
		NewImplementFilterRule(),
	}
	p := NewPlanner(rules, nil)
	if _, conv := p.Explore(ref); !conv {
		t.Fatal("planner did not converge")
	}

	// Look for a physicalFilterWrapper in the top-level Reference.
	foundPhysFilter := false
	for _, m := range ref.Members() {
		if _, ok := m.(*physicalFilterWrapper); ok {
			foundPhysFilter = true
			break
		}
	}
	if !foundPhysFilter {
		t.Fatalf("planner did not produce a physical FilterPlan wrapper after Batch A rules; %d members", len(ref.Members()))
	}
}
