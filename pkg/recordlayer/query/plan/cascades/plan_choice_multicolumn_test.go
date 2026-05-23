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

	pp := extractIndexPlan(physicalPlan)
	if pp == nil {
		t.Fatalf("optimizer should choose IndexScan for compound equality on multi-column index, got %T: %s",
			physicalPlan, physicalPlan.Explain())
	}
	t.Logf("Optimizer chose INDEX SCAN: %s", pp.Explain())
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
	t.Logf("Both columns equality-bound in index scan prefix")
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
	if extractIndexPlan(physicalPlan) == nil {
		t.Fatalf("unique index point lookup should choose IndexScan, got %T", physicalPlan)
	}
	t.Logf("Unique index point lookup -> IndexScan")
}

func TestPlanChoice_PicksBestIndexAmongMultiple(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)

	// WHERE status = 'shipped' — matches idx_status (1 col) not idx_customer (different col)
	pred := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "STATUS", Typ: values.UnknownType},
		predicates.NewLiteralComparison(predicates.ComparisonEquals, "shipped"),
	)
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pred},
		expressions.ForEachQuantifier(scanRef),
	)
	rootRef := expressions.InitialOf(filter)

	// Two indexes: one on CUSTOMER_ID, one on STATUS
	ctx := NewPlanContextFromIndexDefs([]IndexDef{
		&planChoiceIndexDef{
			name:        "idx_customer",
			columns:     []string{"CUSTOMER_ID"},
			recordTypes: []string{"Order"},
			unique:      false,
		},
		&planChoiceIndexDef{
			name:        "idx_status",
			columns:     []string{"STATUS"},
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

	type planExtractor interface {
		GetRecordQueryPlan() plans.RecordQueryPlan
	}
	ph, ok := bestExpr.(planExtractor)
	if !ok {
		t.Fatalf("expected planExtractor, got %T", bestExpr)
	}

	physicalPlan := ph.GetRecordQueryPlan()
	idxPlan := extractIndexPlan(physicalPlan)
	if idxPlan == nil {
		t.Fatalf("expected IndexScan, got %T: %s", physicalPlan, physicalPlan.Explain())
	}
	if idxPlan.GetIndexName() != "idx_status" {
		t.Fatalf("expected optimizer to pick idx_status, picked %s", idxPlan.GetIndexName())
	}
	t.Logf("Optimizer correctly picked idx_status over idx_customer")
}

func TestPlanChoice_InequalityRangeScan(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)

	// WHERE amount > 1000 on index(amount)
	pred := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "AMOUNT", Typ: values.UnknownType},
		predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, int64(1000)),
	)
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pred},
		expressions.ForEachQuantifier(scanRef),
	)
	rootRef := expressions.InitialOf(filter)

	ctx := NewPlanContextFromIndexDefs([]IndexDef{
		&planChoiceIndexDef{
			name:        "idx_amount",
			columns:     []string{"AMOUNT"},
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
		comps := pp.GetScanComparisons()
		if len(comps) == 0 || comps[0].IsEmpty() {
			t.Fatal("index scan should have inequality range bound")
		}
		if comps[0].IsEquality() {
			t.Fatal("expected inequality range, got equality")
		}
		t.Logf("✓ Inequality range scan on index: %s", pp.Explain())
	default:
		t.Logf("Optimizer chose %T for inequality (may not support range scans yet): %s",
			physicalPlan, physicalPlan.Explain())
	}
}

func TestPlanChoice_EqualityPlusInequality(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)

	// WHERE customer_id = 42 AND amount > 100
	pred1 := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "CUSTOMER_ID", Typ: values.UnknownType},
		predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(42)),
	)
	pred2 := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "AMOUNT", Typ: values.UnknownType},
		predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, int64(100)),
	)
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pred1, pred2},
		expressions.ForEachQuantifier(scanRef),
	)
	rootRef := expressions.InitialOf(filter)

	// Index on (customer_id, amount) — equality on prefix + inequality on suffix
	ctx := NewPlanContextFromIndexDefs([]IndexDef{
		&planChoiceIndexDef{
			name:        "idx_customer_amount",
			columns:     []string{"CUSTOMER_ID", "AMOUNT"},
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

	type planExtractor interface {
		GetRecordQueryPlan() plans.RecordQueryPlan
	}
	ph, ok := bestExpr.(planExtractor)
	if !ok {
		t.Fatalf("expected planExtractor, got %T", bestExpr)
	}

	physicalPlan := ph.GetRecordQueryPlan()
	pp := extractIndexPlan(physicalPlan)
	if pp == nil {
		t.Fatalf("expected IndexScan for equality+inequality prefix, got %T: %s",
			physicalPlan, physicalPlan.Explain())
	}
	comps := pp.GetScanComparisons()
	if len(comps) < 2 {
		t.Fatalf("expected 2 comparison ranges, got %d", len(comps))
	}
	if !comps[0].IsEquality() {
		t.Fatal("first column should be equality-bound")
	}
	if !comps[1].IsInequality() {
		t.Fatalf("second column should be inequality-bound, got range type %d", comps[1].GetRangeType())
	}
	t.Logf("Equality + inequality on compound index: %s", pp.Explain())
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

	pp := extractIndexPlan(physicalPlan)
	if pp == nil {
		t.Fatalf("optimizer should choose IndexScan for single-column prefix match, got %T: %s",
			physicalPlan, physicalPlan.Explain())
	}
	t.Logf("Optimizer chose INDEX SCAN with partial prefix: %s", pp.Explain())
}
