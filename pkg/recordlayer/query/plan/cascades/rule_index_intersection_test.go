package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// TestIndexIntersection_TwoCandidatesFullCoverage tests the basic
// 2-way intersection: predicates on two different columns, each with
// its own index. Together they cover all predicates.
func TestIndexIntersection_TwoCandidatesFullCoverage(t *testing.T) {
	t.Parallel()

	a1 := values.UniqueCorrelationIdentifier()
	b1 := values.UniqueCorrelationIdentifier()
	candA := NewValueIndexScanMatchCandidate(
		"Order$status",
		[]string{"Order"},
		[]string{"STATUS"},
		[]values.CorrelationIdentifier{a1},
		values.UnknownType,
		false,
		nil,
	)
	candB := NewValueIndexScanMatchCandidate(
		"Order$amount",
		[]string{"Order"},
		[]string{"AMOUNT"},
		[]values.CorrelationIdentifier{b1},
		values.UnknownType,
		false,
		nil,
	)
	ctx := &indexTestPlanContext{candidates: []MatchCandidate{candA, candB}}

	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "STATUS", Typ: values.TypeString},
				predicates.NewLiteralComparison(predicates.ComparisonEquals, "active"),
			),
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "AMOUNT", Typ: values.TypeInt},
				predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, int64(100)),
			),
		},
		q,
	)
	filterRef := expressions.InitialOf(filter)

	rule := NewIndexIntersectionRule()
	results := FireExpressionRuleWithMemo(rule, filterRef, ctx, nil)

	if len(results) != 1 {
		t.Fatalf("expected 1 yield (2-way intersection), got %d", len(results))
	}
	intr, ok := results[0].(*expressions.LogicalIntersectionExpression)
	if !ok {
		t.Fatalf("expected LogicalIntersectionExpression, got %T", results[0])
	}
	if len(intr.GetQuantifiers()) != 2 {
		t.Fatalf("expected 2 intersection children, got %d", len(intr.GetQuantifiers()))
	}
}

// TestIndexIntersection_SingleCandidateNoFire verifies that when only
// one candidate exists, no intersection is produced (single-index path
// is handled by ImplementIndexScanRule).
func TestIndexIntersection_SingleCandidateNoFire(t *testing.T) {
	t.Parallel()

	a1 := values.UniqueCorrelationIdentifier()
	cand := NewValueIndexScanMatchCandidate(
		"Order$status",
		[]string{"Order"},
		[]string{"STATUS"},
		[]values.CorrelationIdentifier{a1},
		values.UnknownType,
		false,
		nil,
	)
	ctx := &indexTestPlanContext{candidates: []MatchCandidate{cand}}

	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "STATUS", Typ: values.TypeString},
				predicates.NewLiteralComparison(predicates.ComparisonEquals, "active"),
			),
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "AMOUNT", Typ: values.TypeInt},
				predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(50)),
			),
		},
		q,
	)
	filterRef := expressions.InitialOf(filter)

	rule := NewIndexIntersectionRule()
	results := FireExpressionRuleWithMemo(rule, filterRef, ctx, nil)

	if len(results) != 0 {
		t.Fatalf("expected 0 yields (single candidate), got %d", len(results))
	}
}

// TestIndexIntersection_OverlappingPredicates verifies no intersection
// when both candidates consume the same predicate (non-disjoint).
func TestIndexIntersection_OverlappingPredicates(t *testing.T) {
	t.Parallel()

	a1 := values.UniqueCorrelationIdentifier()
	b1 := values.UniqueCorrelationIdentifier()
	candA := NewValueIndexScanMatchCandidate(
		"Order$status",
		[]string{"Order"},
		[]string{"STATUS"},
		[]values.CorrelationIdentifier{a1},
		values.UnknownType,
		false,
		nil,
	)
	candB := NewValueIndexScanMatchCandidate(
		"Order$status_v2",
		[]string{"Order"},
		[]string{"STATUS"},
		[]values.CorrelationIdentifier{b1},
		values.UnknownType,
		false,
		nil,
	)
	ctx := &indexTestPlanContext{candidates: []MatchCandidate{candA, candB}}

	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "STATUS", Typ: values.TypeString},
				predicates.NewLiteralComparison(predicates.ComparisonEquals, "active"),
			),
		},
		q,
	)
	filterRef := expressions.InitialOf(filter)

	rule := NewIndexIntersectionRule()
	results := FireExpressionRuleWithMemo(rule, filterRef, ctx, nil)

	if len(results) != 0 {
		t.Fatalf("expected 0 yields (overlapping predicates), got %d", len(results))
	}
}

// TestIndexIntersection_PartialCoverage verifies that when two
// candidates cover some but not all predicates, a filter-over-
// intersection is produced (residual predicate wraps the intersection).
func TestIndexIntersection_PartialCoverage(t *testing.T) {
	t.Parallel()

	a1 := values.UniqueCorrelationIdentifier()
	b1 := values.UniqueCorrelationIdentifier()
	candA := NewValueIndexScanMatchCandidate(
		"Order$status",
		[]string{"Order"},
		[]string{"STATUS"},
		[]values.CorrelationIdentifier{a1},
		values.UnknownType,
		false,
		nil,
	)
	candB := NewValueIndexScanMatchCandidate(
		"Order$amount",
		[]string{"Order"},
		[]string{"AMOUNT"},
		[]values.CorrelationIdentifier{b1},
		values.UnknownType,
		false,
		nil,
	)
	ctx := &indexTestPlanContext{candidates: []MatchCandidate{candA, candB}}

	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)
	// 3 predicates: status + amount + date. The two candidates cover
	// status + amount but not date → Filter(date, Intersection).
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "STATUS", Typ: values.TypeString},
				predicates.NewLiteralComparison(predicates.ComparisonEquals, "active"),
			),
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "AMOUNT", Typ: values.TypeInt},
				predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(50)),
			),
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "DATE", Typ: values.TypeString},
				predicates.NewLiteralComparison(predicates.ComparisonEquals, "2024-01-01"),
			),
		},
		q,
	)
	filterRef := expressions.InitialOf(filter)

	rule := NewIndexIntersectionRule()
	results := FireExpressionRuleWithMemo(rule, filterRef, ctx, nil)

	if len(results) != 1 {
		t.Fatalf("expected 1 yield (filter over intersection), got %d", len(results))
	}
	// Should be a filter wrapping an intersection.
	filterResult, ok := results[0].(*expressions.LogicalFilterExpression)
	if !ok {
		t.Fatalf("expected LogicalFilterExpression wrapping intersection, got %T", results[0])
	}
	// Residual should be the DATE predicate.
	residualPreds := filterResult.GetPredicates()
	if len(residualPreds) != 1 {
		t.Fatalf("expected 1 residual predicate, got %d", len(residualPreds))
	}
}

// TestIndexIntersection_ThreeWay verifies 3-way intersection: three
// predicates on three columns, each with its own index. Together they
// cover all predicates via a 3-way intersection.
func TestIndexIntersection_ThreeWay(t *testing.T) {
	t.Parallel()

	a1 := values.UniqueCorrelationIdentifier()
	b1 := values.UniqueCorrelationIdentifier()
	c1 := values.UniqueCorrelationIdentifier()
	candA := NewValueIndexScanMatchCandidate(
		"Order$status",
		[]string{"Order"},
		[]string{"STATUS"},
		[]values.CorrelationIdentifier{a1},
		values.UnknownType,
		false,
		nil,
	)
	candB := NewValueIndexScanMatchCandidate(
		"Order$amount",
		[]string{"Order"},
		[]string{"AMOUNT"},
		[]values.CorrelationIdentifier{b1},
		values.UnknownType,
		false,
		nil,
	)
	candC := NewValueIndexScanMatchCandidate(
		"Order$date",
		[]string{"Order"},
		[]string{"DATE"},
		[]values.CorrelationIdentifier{c1},
		values.UnknownType,
		false,
		nil,
	)
	ctx := &indexTestPlanContext{candidates: []MatchCandidate{candA, candB, candC}}

	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "STATUS", Typ: values.TypeString},
				predicates.NewLiteralComparison(predicates.ComparisonEquals, "active"),
			),
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "AMOUNT", Typ: values.TypeInt},
				predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(50)),
			),
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "DATE", Typ: values.TypeString},
				predicates.NewLiteralComparison(predicates.ComparisonEquals, "2024-01-01"),
			),
		},
		q,
	)
	filterRef := expressions.InitialOf(filter)

	rule := NewIndexIntersectionRule()
	results := FireExpressionRuleWithMemo(rule, filterRef, ctx, nil)

	// Should produce 2-way intersections (3 pairs) AND a 3-way intersection.
	found3Way := false
	for _, r := range results {
		intr, ok := r.(*expressions.LogicalIntersectionExpression)
		if !ok {
			continue
		}
		if len(intr.GetQuantifiers()) == 3 {
			found3Way = true
			break
		}
	}
	if !found3Way {
		t.Fatalf("expected a 3-way intersection, got %d results (none with 3 legs)", len(results))
	}
}

// TestIndexIntersection_PlannerIntegration verifies the full pipeline:
// 2-way intersection → ImplementIntersectionRule → physical plan.
func TestIndexIntersection_PlannerIntegration(t *testing.T) {
	t.Parallel()

	a1 := values.UniqueCorrelationIdentifier()
	b1 := values.UniqueCorrelationIdentifier()
	candA := NewValueIndexScanMatchCandidate(
		"Order$status",
		[]string{"Order"},
		[]string{"STATUS"},
		[]values.CorrelationIdentifier{a1},
		values.UnknownType,
		false,
		nil,
	)
	candB := NewValueIndexScanMatchCandidate(
		"Order$amount",
		[]string{"Order"},
		[]string{"AMOUNT"},
		[]values.CorrelationIdentifier{b1},
		values.UnknownType,
		false,
		nil,
	)
	ctx := &indexTestPlanContext{candidates: []MatchCandidate{candA, candB}}

	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "STATUS", Typ: values.TypeString},
				predicates.NewLiteralComparison(predicates.ComparisonEquals, "active"),
			),
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "AMOUNT", Typ: values.TypeInt},
				predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(50)),
			),
		},
		q,
	)
	ref := expressions.InitialOf(filter)

	rules := append(DefaultExpressionRules(), BatchAExpressionRules()...)
	p := NewPlanner(rules, ctx).WithImplementationRules(DefaultImplementationRules())
	if _, _, err := p.Plan(ref); err != nil {
		t.Fatalf("Plan: %v", err)
	}

	// The planner should produce at least one physical intersection.
	foundIntersection := false
	var walk func(r *expressions.Reference, visited map[*expressions.Reference]bool)
	walk = func(r *expressions.Reference, visited map[*expressions.Reference]bool) {
		if r == nil || visited[r] {
			return
		}
		visited[r] = true
		for _, m := range r.AllMembers() {
			if IsPhysicalIntersection(m) {
				foundIntersection = true
				return
			}
			for _, qq := range m.GetQuantifiers() {
				walk(qq.GetRangesOver(), visited)
				if foundIntersection {
					return
				}
			}
		}
	}
	walk(ref, map[*expressions.Reference]bool{})
	if !foundIntersection {
		t.Fatal("planner did not produce a physical intersection anywhere")
	}
}
