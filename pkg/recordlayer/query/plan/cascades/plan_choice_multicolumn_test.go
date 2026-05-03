package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

func TestPlanChoice_MultiColumnIndexPrefix(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)

	// WHERE customer_id = 42 AND status = 'shipped'
	pred1 := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "CUSTOMER_ID", Typ: values.UnknownType},
		predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(42)),
	)
	pred2 := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "STATUS", Typ: values.UnknownType},
		predicates.NewLiteralComparison(predicates.ComparisonEquals, "shipped"),
	)
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pred1, pred2},
		expressions.ForEachQuantifier(scanRef),
	)
	rootRef := expressions.InitialOf(filter)

	// Index on (customer_id, status)
	ctx := NewPlanContextFromIndexDefs([]IndexDef{
		&planChoiceIndexDef{
			name:        "idx_customer_status",
			columns:     []string{"CUSTOMER_ID", "STATUS"},
			recordTypes: []string{"Order"},
			unique:      false,
		},
	})

	rules := append(DefaultExpressionRules(), BatchAExpressionRules()...)
	p := NewPlanner(rules, ctx).
		WithImplementationRules(DefaultImplementationRules())
	bestExpr, _, err := p.Plan(rootRef)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if bestExpr == nil {
		t.Fatal("Plan returned nil")
	}

	type planExtractor interface {
		GetRecordQueryPlan() plans.RecordQueryPlan
	}
	ph, ok := bestExpr.(planExtractor)
	if !ok {
		t.Fatalf("expected planExtractor, got %T", bestExpr)
	}

	physicalPlan := ph.GetRecordQueryPlan()

	switch pp := physicalPlan.(type) {
	case *plans.RecordQueryIndexPlan:
		t.Logf("✓ Optimizer chose INDEX SCAN: %s", pp.Explain())
		comps := pp.GetScanComparisons()
		eqCount := 0
		for _, c := range comps {
			if c.IsEquality() {
				eqCount++
			}
		}
		if eqCount < 2 {
			t.Fatalf("expected 2 equality-bound prefix columns, got %d", eqCount)
		}
		t.Logf("✓ Both columns equality-bound in index scan prefix")
	default:
		t.Fatalf("optimizer should choose IndexScan for compound equality on multi-column index, got %T: %s",
			physicalPlan, physicalPlan.Explain())
	}
}

func TestPlanChoice_NoIndexForNonMatchingColumn(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)

	// WHERE total > 500 — no index on "total"
	pred := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "TOTAL", Typ: values.UnknownType},
		predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, int64(500)),
	)
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pred},
		expressions.ForEachQuantifier(scanRef),
	)
	rootRef := expressions.InitialOf(filter)

	// Index on (customer_id) — NOT on total
	ctx := NewPlanContextFromIndexDefs([]IndexDef{
		&planChoiceIndexDef{
			name:        "idx_customer",
			columns:     []string{"CUSTOMER_ID"},
			recordTypes: []string{"Order"},
			unique:      false,
		},
	})

	rules := append(DefaultExpressionRules(), BatchAExpressionRules()...)
	p := NewPlanner(rules, ctx).
		WithImplementationRules(DefaultImplementationRules())
	bestExpr, _, err := p.Plan(rootRef)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if bestExpr == nil {
		t.Fatal("Plan returned nil")
	}

	type planExtractor interface {
		GetRecordQueryPlan() plans.RecordQueryPlan
	}
	ph, ok := bestExpr.(planExtractor)
	if !ok {
		t.Fatalf("expected planExtractor, got %T", bestExpr)
	}

	physicalPlan := ph.GetRecordQueryPlan()

	// Should NOT be an IndexScan since no index covers the "TOTAL" column
	if _, ok := physicalPlan.(*plans.RecordQueryIndexPlan); ok {
		t.Fatal("optimizer should NOT choose IndexScan when predicate doesn't match any index column")
	}
	t.Logf("✓ Optimizer correctly chose full scan + filter (no matching index): %T", physicalPlan)
}

func TestPlanChoice_UniqueIndexPointLookup(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"User"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)

	// WHERE email = 'user@example.com' on UNIQUE index
	pred := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "EMAIL", Typ: values.UnknownType},
		predicates.NewLiteralComparison(predicates.ComparisonEquals, "user@example.com"),
	)
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pred},
		expressions.ForEachQuantifier(scanRef),
	)
	rootRef := expressions.InitialOf(filter)

	ctx := NewPlanContextFromIndexDefs([]IndexDef{
		&planChoiceIndexDef{
			name:        "idx_email_unique",
			columns:     []string{"EMAIL"},
			recordTypes: []string{"User"},
			unique:      true,
		},
	})

	rules := append(DefaultExpressionRules(), BatchAExpressionRules()...)
	p := NewPlanner(rules, ctx).
		WithImplementationRules(DefaultImplementationRules())
	bestExpr, _, err := p.Plan(rootRef)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	type planExtractor interface {
		GetRecordQueryPlan() plans.RecordQueryPlan
	}
	ph, ok := bestExpr.(planExtractor)
	if !ok {
		t.Fatalf("expected planExtractor, got %T", bestExpr)
	}

	physicalPlan := ph.GetRecordQueryPlan()
	if _, ok := physicalPlan.(*plans.RecordQueryIndexPlan); !ok {
		t.Fatalf("unique index point lookup should choose IndexScan, got %T", physicalPlan)
	}
	t.Logf("✓ Unique index point lookup → IndexScan")
}

func TestPlanChoice_PartialPrefixMatch(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)

	// WHERE customer_id = 42 (only first column of 2-column index)
	pred := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "CUSTOMER_ID", Typ: values.UnknownType},
		predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(42)),
	)
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pred},
		expressions.ForEachQuantifier(scanRef),
	)
	rootRef := expressions.InitialOf(filter)

	ctx := NewPlanContextFromIndexDefs([]IndexDef{
		&planChoiceIndexDef{
			name:        "idx_customer_status",
			columns:     []string{"CUSTOMER_ID", "STATUS"},
			recordTypes: []string{"Order"},
			unique:      false,
		},
	})

	rules := append(DefaultExpressionRules(), BatchAExpressionRules()...)
	p := NewPlanner(rules, ctx).
		WithImplementationRules(DefaultImplementationRules())
	bestExpr, _, err := p.Plan(rootRef)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if bestExpr == nil {
		t.Fatal("Plan returned nil")
	}

	type planExtractor interface {
		GetRecordQueryPlan() plans.RecordQueryPlan
	}
	ph, ok := bestExpr.(planExtractor)
	if !ok {
		t.Fatalf("expected planExtractor, got %T", bestExpr)
	}

	physicalPlan := ph.GetRecordQueryPlan()

	switch pp := physicalPlan.(type) {
	case *plans.RecordQueryIndexPlan:
		t.Logf("✓ Optimizer chose INDEX SCAN with partial prefix: %s", pp.Explain())
	default:
		t.Fatalf("optimizer should choose IndexScan for single-column prefix match, got %T: %s",
			physicalPlan, physicalPlan.Explain())
	}
}
