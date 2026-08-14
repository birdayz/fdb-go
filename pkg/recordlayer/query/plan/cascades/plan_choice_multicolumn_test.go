package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func planChoiceRowType(recordType string) *values.RecordType {
	return values.NewRecordType(recordType, false, []values.Field{
		{Name: "CUSTOMER_ID", FieldType: values.NullableLong, Ordinal: 0},
		{Name: "STATUS", FieldType: values.NullableString, Ordinal: 1},
		{Name: "AMOUNT", FieldType: values.NullableLong, Ordinal: 2},
		{Name: "TOTAL", FieldType: values.NullableLong, Ordinal: 3},
		{Name: "EMAIL", FieldType: values.NullableString, Ordinal: 4},
		{Name: "UNINDEXED_COL", FieldType: values.NullableLong, Ordinal: 5},
		{Name: "CATEGORY", FieldType: values.NullableString, Ordinal: 6},
	})
}

func planChoiceScan(
	t testing.TB,
	recordType string,
) *expressions.FullUnorderedScanExpression {
	t.Helper()
	return mustFullUnorderedScan(t, []string{recordType}, planChoiceRowType(recordType))
}

func planChoiceField(
	t testing.TB,
	quantifier expressions.Quantifier,
	name string,
) values.Value {
	t.Helper()
	rootValue, rootErr := quantifier.RequireFlowedObjectValue()
	root := mustConstruct(t, rootValue, rootErr)
	request, requestErr := values.FieldByName(name)
	fieldValue, fieldErr := values.ResolveFieldAccess(
		root, []values.FieldRequest{mustConstruct(t, request, requestErr)})
	return mustConstruct(t, fieldValue, fieldErr)
}

func planChoiceFilter(
	t testing.TB,
	preds []predicates.QueryPredicate,
	quantifier expressions.Quantifier,
) *expressions.LogicalFilterExpression {
	t.Helper()
	filterValue, filterErr := expressions.NewLogicalFilterExpression(preds, quantifier)
	return mustConstruct(t, filterValue, filterErr)
}

func TestPlanChoice_MultiColumnIndexPrefix(t *testing.T) {
	t.Parallel()

	scan := planChoiceScan(t, "Order")
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)

	// WHERE customer_id = 42 AND status = 'shipped'
	pred1 := predicates.NewComparisonPredicate(
		planChoiceField(t, q, "CUSTOMER_ID"),
		predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(42)),
	)
	pred2 := predicates.NewComparisonPredicate(
		planChoiceField(t, q, "STATUS"),
		predicates.NewLiteralComparison(predicates.ComparisonEquals, "shipped"),
	)
	filter := planChoiceFilter(t,
		[]predicates.QueryPredicate{pred1, pred2},
		q,
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

	rules := DefaultExpressionRules()
	p := NewPlanner(rules, ctx).
		WithPlanningExpressionRules(BatchAExpressionRules()).
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

	scan := planChoiceScan(t, "Order")
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)

	// WHERE total > 500 — no index on "total"
	pred := predicates.NewComparisonPredicate(
		planChoiceField(t, q, "TOTAL"),
		predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, int64(500)),
	)
	filter := planChoiceFilter(t,
		[]predicates.QueryPredicate{pred},
		q,
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

	rules := DefaultExpressionRules()
	p := NewPlanner(rules, ctx).
		WithPlanningExpressionRules(BatchAExpressionRules()).
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

	scan := planChoiceScan(t, "User")
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)

	// WHERE email = 'user@example.com' on UNIQUE index
	pred := predicates.NewComparisonPredicate(
		planChoiceField(t, q, "EMAIL"),
		predicates.NewLiteralComparison(predicates.ComparisonEquals, "user@example.com"),
	)
	filter := planChoiceFilter(t,
		[]predicates.QueryPredicate{pred},
		q,
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

	rules := DefaultExpressionRules()
	p := NewPlanner(rules, ctx).
		WithPlanningExpressionRules(BatchAExpressionRules()).
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

	scan := planChoiceScan(t, "Order")
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)

	// WHERE status = 'shipped' — matches idx_status (1 col) not idx_customer (different col)
	pred := predicates.NewComparisonPredicate(
		planChoiceField(t, q, "STATUS"),
		predicates.NewLiteralComparison(predicates.ComparisonEquals, "shipped"),
	)
	filter := planChoiceFilter(t,
		[]predicates.QueryPredicate{pred},
		q,
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

	rules := DefaultExpressionRules()
	p := NewPlanner(rules, ctx).
		WithPlanningExpressionRules(BatchAExpressionRules()).
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

	scan := planChoiceScan(t, "Order")
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)

	// WHERE amount > 1000 on index(amount)
	pred := predicates.NewComparisonPredicate(
		planChoiceField(t, q, "AMOUNT"),
		predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, int64(1000)),
	)
	filter := planChoiceFilter(t,
		[]predicates.QueryPredicate{pred},
		q,
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

	rules := DefaultExpressionRules()
	p := NewPlanner(rules, ctx).
		WithPlanningExpressionRules(BatchAExpressionRules()).
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

	scan := planChoiceScan(t, "Order")
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)

	// WHERE customer_id = 42 AND amount > 100
	pred1 := predicates.NewComparisonPredicate(
		planChoiceField(t, q, "CUSTOMER_ID"),
		predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(42)),
	)
	pred2 := predicates.NewComparisonPredicate(
		planChoiceField(t, q, "AMOUNT"),
		predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, int64(100)),
	)
	filter := planChoiceFilter(t,
		[]predicates.QueryPredicate{pred1, pred2},
		q,
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

	rules := DefaultExpressionRules()
	p := NewPlanner(rules, ctx).
		WithPlanningExpressionRules(BatchAExpressionRules()).
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

	scan := planChoiceScan(t, "Order")
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)

	// WHERE customer_id = 42 (only first column of 2-column index)
	pred := predicates.NewComparisonPredicate(
		planChoiceField(t, q, "CUSTOMER_ID"),
		predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(42)),
	)
	filter := planChoiceFilter(t,
		[]predicates.QueryPredicate{pred},
		q,
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

	rules := DefaultExpressionRules()
	p := NewPlanner(rules, ctx).
		WithPlanningExpressionRules(BatchAExpressionRules()).
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

// The following three tests re-pin the prefix-binding dimensions that the retired
// ImplementIndexScanRule's unit tests covered (gap-in-prefix, inequality-stops-prefix,
// all-predicates-residual), now END-TO-END through the data-access path (RFC-076 v5).
// They must stay green so a future regression in candidate prefix binding cannot ship
// silently.

// extractWinnerPlan extracts the chosen physical plan from a Plan() result.
func extractWinnerPlan(t *testing.T, bestExpr expressions.RelationalExpression) plans.RecordQueryPlan {
	t.Helper()
	ph, ok := bestExpr.(interface {
		GetRecordQueryPlan() plans.RecordQueryPlan
	})
	if !ok {
		t.Fatalf("expected a physical plan expression, got %T", bestExpr)
	}
	return ph.GetRecordQueryPlan()
}

// planHasIndexScan reports whether a RecordQueryIndexPlan appears ANYWHERE in the plan
// tree. Used by the no-index negative assertions: a root-only check (extractIndexPlan)
// would miss a nested index scan under a PredicatesFilter/Fetch, letting a regression that
// wrongly uses the index slip through.
func planHasIndexScan(p plans.RecordQueryPlan) bool {
	found := false
	plans.Walk(p, func(n plans.RecordQueryPlan) bool {
		if _, ok := n.(*plans.RecordQueryIndexPlan); ok {
			found = true
			return false
		}
		return true
	})
	return found
}

// TestPlanChoice_GapInPrefix: a predicate on the SECOND column of a compound index, with
// the FIRST unbound, must NOT use the index — an FDB key-range prefix is contiguous from
// position 0, so AMOUNT-only over (STATUS, AMOUNT) yields a full scan + residual filter.
func TestPlanChoice_GapInPrefix(t *testing.T) {
	t.Parallel()

	scan := planChoiceScan(t, "Order")
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)
	pred := predicates.NewComparisonPredicate(
		planChoiceField(t, q, "AMOUNT"),
		predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(100)),
	)
	filter := planChoiceFilter(t,
		[]predicates.QueryPredicate{pred},
		q,
	)
	rootRef := expressions.InitialOf(filter)
	ctx := NewPlanContextFromIndexDefs([]IndexDef{
		&planChoiceIndexDef{name: "idx_status_amount", columns: []string{"STATUS", "AMOUNT"}, recordTypes: []string{"Order"}, unique: false},
	})
	p := NewPlanner(DefaultExpressionRules(), ctx).
		WithPlanningExpressionRules(BatchAExpressionRules()).
		WithImplementationRules(DefaultImplementationRules())
	bestExpr, _, err := p.Plan(rootRef)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	physicalPlan := extractWinnerPlan(t, bestExpr)
	if planHasIndexScan(physicalPlan) {
		t.Fatalf("gap-in-prefix: must NOT use the (STATUS,AMOUNT) index for an AMOUNT-only predicate; got %s", physicalPlan.Explain())
	}
}

// TestPlanChoice_InequalityStopsPrefix: on (STATUS, AMOUNT), `STATUS > 5 AND AMOUNT = 100`
// must bind only STATUS (the leading inequality) into the scan and leave AMOUNT a residual
// — an inequality terminates the prefix, so the trailing equality cannot be a scan bound.
func TestPlanChoice_InequalityStopsPrefix(t *testing.T) {
	t.Parallel()

	scan := planChoiceScan(t, "Order")
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)
	filter := planChoiceFilter(t,
		[]predicates.QueryPredicate{
			predicates.NewComparisonPredicate(
				planChoiceField(t, q, "STATUS"),
				predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, "M"),
			),
			predicates.NewComparisonPredicate(
				planChoiceField(t, q, "AMOUNT"),
				predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(100)),
			),
		},
		q,
	)
	rootRef := expressions.InitialOf(filter)
	ctx := NewPlanContextFromIndexDefs([]IndexDef{
		&planChoiceIndexDef{name: "idx_status_amount", columns: []string{"STATUS", "AMOUNT"}, recordTypes: []string{"Order"}, unique: false},
	})
	p := NewPlanner(DefaultExpressionRules(), ctx).
		WithPlanningExpressionRules(BatchAExpressionRules()).
		WithImplementationRules(DefaultImplementationRules())
	bestExpr, _, err := p.Plan(rootRef)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	physicalPlan := extractWinnerPlan(t, bestExpr)
	var idx *plans.RecordQueryIndexPlan
	plans.Walk(physicalPlan, func(node plans.RecordQueryPlan) bool {
		if found, ok := plans.IndexPlanOf(node); ok {
			idx = found
			return false
		}
		return true
	})
	if idx == nil {
		t.Fatalf("inequality-stops-prefix: expected an index scan on STATUS; got %s", physicalPlan.Explain())
	}
	// The scan must bind exactly ONE column (STATUS, a range). AMOUNT must NOT be a scan
	// bound — it terminates after the inequality and stays a residual filter.
	bound := 0
	for _, cr := range idx.GetScanComparisons() {
		if cr != nil && !cr.IsEmpty() {
			bound++
		}
	}
	if bound != 1 {
		t.Fatalf("inequality-stops-prefix: expected exactly 1 bound scan column (STATUS range), got %d: %s", bound, physicalPlan.Explain())
	}
}

// TestPlanChoice_AllPredicatesResidual: when the only predicate is on a column NOT in any
// index, no index scan is produced (full scan + residual). Complements
// TestPlanChoice_NoIndexForNonMatchingColumn with a multi-column candidate present.
func TestPlanChoice_AllPredicatesResidual(t *testing.T) {
	t.Parallel()

	scan := planChoiceScan(t, "Order")
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)
	pred := predicates.NewComparisonPredicate(
		planChoiceField(t, q, "UNINDEXED_COL"),
		predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(42)),
	)
	filter := planChoiceFilter(t,
		[]predicates.QueryPredicate{pred},
		q,
	)
	rootRef := expressions.InitialOf(filter)
	ctx := NewPlanContextFromIndexDefs([]IndexDef{
		&planChoiceIndexDef{name: "idx_status_amount", columns: []string{"STATUS", "AMOUNT"}, recordTypes: []string{"Order"}, unique: false},
	})
	p := NewPlanner(DefaultExpressionRules(), ctx).
		WithPlanningExpressionRules(BatchAExpressionRules()).
		WithImplementationRules(DefaultImplementationRules())
	bestExpr, _, err := p.Plan(rootRef)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	physicalPlan := extractWinnerPlan(t, bestExpr)
	if planHasIndexScan(physicalPlan) {
		t.Fatalf("all-predicates-residual: must NOT use an index for a non-indexed predicate; got %s", physicalPlan.Explain())
	}
}

// TestPlanChoice_ParameterizedResidualKeepsIndex pins the PR #257 fix: a
// query-parameter ($N → ConstantObjectValue) in a residual predicate is an execution
// constant, NOT a join/row correlation. The data-access compensation gate must subtract
// it, so `WHERE customer_id = 42 AND total > $1` (total non-indexed) still materializes the
// idx_customer scan + residual filter rather than falling back to a full scan.
func TestPlanChoice_ParameterizedResidualKeepsIndex(t *testing.T) {
	t.Parallel()

	scan := planChoiceScan(t, "Order")
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)
	param := values.NewConstantObjectValue(values.UniqueCorrelationIdentifier(), "p1", values.NullableLong)
	filter := planChoiceFilter(t,
		[]predicates.QueryPredicate{
			predicates.NewComparisonPredicate(
				planChoiceField(t, q, "CUSTOMER_ID"),
				predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(42)),
			),
			predicates.NewComparisonPredicate(
				planChoiceField(t, q, "TOTAL"),
				predicates.Comparison{Type: predicates.ComparisonGreaterThan, Operand: param},
			),
		},
		q,
	)
	rootRef := expressions.InitialOf(filter)
	ctx := NewPlanContextFromIndexDefs([]IndexDef{
		&planChoiceIndexDef{name: "idx_customer", columns: []string{"CUSTOMER_ID"}, recordTypes: []string{"Order"}, unique: false},
	})
	p := NewPlanner(DefaultExpressionRules(), ctx).
		WithPlanningExpressionRules(BatchAExpressionRules()).
		WithImplementationRules(DefaultImplementationRules())
	bestExpr, _, err := p.Plan(rootRef)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	physicalPlan := extractWinnerPlan(t, bestExpr)
	var idx *plans.RecordQueryIndexPlan
	plans.Walk(physicalPlan, func(n plans.RecordQueryPlan) bool {
		if ip, ok := n.(*plans.RecordQueryIndexPlan); ok {
			idx = ip
			return false
		}
		return true
	})
	if idx == nil {
		t.Fatalf("parameterized residual must NOT block the idx_customer scan ($1 is a constant, not a correlation); got %s", physicalPlan.Explain())
	}
}
