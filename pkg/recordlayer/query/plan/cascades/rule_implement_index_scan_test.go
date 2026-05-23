package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

func TestImplementIndexScanRule_SingleEquality(t *testing.T) {
	t.Parallel()

	a1 := values.UniqueCorrelationIdentifier()
	a2 := values.UniqueCorrelationIdentifier()
	cand := NewValueIndexScanMatchCandidate(
		"Order$status",
		[]string{"Order"},
		[]string{"STATUS", "DATE"},
		[]values.CorrelationIdentifier{a1, a2},
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
		},
		q,
	)
	filterRef := expressions.InitialOf(filter)

	rule := NewImplementIndexScanRule()
	results := FireExpressionRuleWithMemo(rule, filterRef, ctx, nil)

	if len(results) != 1 {
		t.Fatalf("expected 1 yield, got %d", len(results))
	}

	wrapper, ok := results[0].(*physicalIndexScanWrapper)
	if !ok {
		t.Fatalf("expected *physicalIndexScanWrapper, got %T", results[0])
	}
	if wrapper.plan.GetIndexName() != "Order$status" {
		t.Fatalf("index=%q, want Order$status", wrapper.plan.GetIndexName())
	}
	comps := wrapper.plan.GetScanComparisons()
	if len(comps) != 2 {
		t.Fatalf("expected 2 scan comparisons, got %d", len(comps))
	}
	if !comps[0].IsEquality() {
		t.Fatal("first comparison should be equality")
	}
	if !comps[1].IsEmpty() {
		t.Fatal("second comparison should be empty")
	}
}

func TestImplementIndexScanRule_MultiEquality_AllConsumed(t *testing.T) {
	t.Parallel()

	a1 := values.UniqueCorrelationIdentifier()
	a2 := values.UniqueCorrelationIdentifier()
	cand := NewValueIndexScanMatchCandidate(
		"Order$status_date",
		[]string{"Order"},
		[]string{"STATUS", "DATE"},
		[]values.CorrelationIdentifier{a1, a2},
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
				&values.FieldValue{Field: "DATE", Typ: values.TypeInt},
				predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(20260101)),
			),
		},
		q,
	)
	filterRef := expressions.InitialOf(filter)

	rule := NewImplementIndexScanRule()
	results := FireExpressionRuleWithMemo(rule, filterRef, ctx, nil)

	if len(results) != 1 {
		t.Fatalf("expected 1 yield, got %d", len(results))
	}
	wrapper, ok := results[0].(*physicalIndexScanWrapper)
	if !ok {
		t.Fatalf("expected *physicalIndexScanWrapper (all consumed), got %T", results[0])
	}
	comps := wrapper.plan.GetScanComparisons()
	if !comps[0].IsEquality() || !comps[1].IsEquality() {
		t.Fatal("both comparisons should be equality")
	}
}

func TestImplementIndexScanRule_ResidualPredicate(t *testing.T) {
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
				predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, int64(100)),
			),
		},
		q,
	)
	filterRef := expressions.InitialOf(filter)

	rule := NewImplementIndexScanRule()
	results := FireExpressionRuleWithMemo(rule, filterRef, ctx, nil)

	if len(results) != 1 {
		t.Fatalf("expected 1 yield, got %d", len(results))
	}
	fw, ok := results[0].(*physicalFilterWrapper)
	if !ok {
		t.Fatalf("expected *physicalFilterWrapper (residual), got %T", results[0])
	}
	residuals := fw.GetPlan().GetPredicates()
	if len(residuals) != 1 {
		t.Fatalf("expected 1 residual predicate, got %d", len(residuals))
	}
	cp, ok := residuals[0].(*predicates.ComparisonPredicate)
	if !ok {
		t.Fatalf("residual should be ComparisonPredicate, got %T", residuals[0])
	}
	fv := cp.Operand.(*values.FieldValue)
	if fv.Field != "AMOUNT" {
		t.Fatalf("residual field=%q, want AMOUNT", fv.Field)
	}
}

func TestImplementIndexScanRule_NoMatchingCandidate(t *testing.T) {
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
				&values.FieldValue{Field: "AMOUNT", Typ: values.TypeInt},
				predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, int64(100)),
			),
		},
		q,
	)
	filterRef := expressions.InitialOf(filter)

	rule := NewImplementIndexScanRule()
	results := FireExpressionRuleWithMemo(rule, filterRef, ctx, nil)

	if len(results) != 0 {
		t.Fatalf("expected 0 yields (no column match), got %d", len(results))
	}
}

func TestImplementIndexScanRule_RecordTypeMismatch(t *testing.T) {
	t.Parallel()

	a1 := values.UniqueCorrelationIdentifier()
	cand := NewValueIndexScanMatchCandidate(
		"Customer$name",
		[]string{"Customer"},
		[]string{"NAME"},
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
				&values.FieldValue{Field: "NAME", Typ: values.TypeString},
				predicates.NewLiteralComparison(predicates.ComparisonEquals, "Alice"),
			),
		},
		q,
	)
	filterRef := expressions.InitialOf(filter)

	rule := NewImplementIndexScanRule()
	results := FireExpressionRuleWithMemo(rule, filterRef, ctx, nil)

	if len(results) != 0 {
		t.Fatalf("expected 0 yields (record type mismatch), got %d", len(results))
	}
}

func TestImplementIndexScanRule_InequalityPrefix(t *testing.T) {
	t.Parallel()

	a1 := values.UniqueCorrelationIdentifier()
	a2 := values.UniqueCorrelationIdentifier()
	cand := NewValueIndexScanMatchCandidate(
		"Order$date_amount",
		[]string{"Order"},
		[]string{"DATE", "AMOUNT"},
		[]values.CorrelationIdentifier{a1, a2},
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
				&values.FieldValue{Field: "DATE", Typ: values.TypeInt},
				predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, int64(20260101)),
			),
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "AMOUNT", Typ: values.TypeInt},
				predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(50)),
			),
		},
		q,
	)
	filterRef := expressions.InitialOf(filter)

	rule := NewImplementIndexScanRule()
	results := FireExpressionRuleWithMemo(rule, filterRef, ctx, nil)

	if len(results) != 1 {
		t.Fatalf("expected 1 yield, got %d", len(results))
	}
	// DATE > 20260101 → inequality on first column → prefix stops after it.
	// AMOUNT = 50 is after the inequality → residual.
	fw, ok := results[0].(*physicalFilterWrapper)
	if !ok {
		t.Fatalf("expected *physicalFilterWrapper (AMOUNT residual), got %T", results[0])
	}
	inner := fw.GetPlan().GetInner()
	idxPlan := extractIndexPlan(inner)
	if idxPlan == nil {
		t.Fatalf("inner should contain a *RecordQueryIndexPlan, got %T", inner)
	}
	comps := idxPlan.GetScanComparisons()
	if !comps[0].IsInequality() {
		t.Fatal("first comparison should be inequality")
	}
	if !comps[1].IsEmpty() {
		t.Fatal("second comparison should be empty (after inequality)")
	}
}

func TestImplementIndexScanRule_NoCandidates(t *testing.T) {
	t.Parallel()
	ctx := EmptyPlanContext()

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

	rule := NewImplementIndexScanRule()
	results := FireExpressionRuleWithMemo(rule, filterRef, ctx, nil)
	if len(results) != 0 {
		t.Fatalf("expected 0 yields (no candidates), got %d", len(results))
	}
}

func TestImplementIndexScanRule_RangeScan_TwoInequalitiesSameColumn(t *testing.T) {
	t.Parallel()

	a1 := values.UniqueCorrelationIdentifier()
	cand := NewValueIndexScanMatchCandidate(
		"Order$amount",
		[]string{"Order"},
		[]string{"AMOUNT"},
		[]values.CorrelationIdentifier{a1},
		values.UnknownType,
		false,
		nil,
	)
	ctx := &indexTestPlanContext{candidates: []MatchCandidate{cand}}

	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)
	// AMOUNT > 5 AND AMOUNT < 100 — both bind to the same column,
	// merge into a single inequality range.
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "AMOUNT", Typ: values.TypeInt},
				predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, int64(5)),
			),
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "AMOUNT", Typ: values.TypeInt},
				predicates.NewLiteralComparison(predicates.ComparisonLessThan, int64(100)),
			),
		},
		q,
	)
	filterRef := expressions.InitialOf(filter)

	rule := NewImplementIndexScanRule()
	results := FireExpressionRuleWithMemo(rule, filterRef, ctx, nil)

	if len(results) != 1 {
		t.Fatalf("expected 1 yield, got %d", len(results))
	}
	// Both predicates consumed (same column, merged range) → bare index scan.
	wrapper, ok := results[0].(*physicalIndexScanWrapper)
	if !ok {
		t.Fatalf("expected *physicalIndexScanWrapper (all consumed), got %T", results[0])
	}
	comps := wrapper.plan.GetScanComparisons()
	if !comps[0].IsInequality() {
		t.Fatal("first comparison should be inequality (merged range)")
	}
}

func TestImplementIndexScanRule_PlannerIntegration_PrefersIndexOverFullScan(t *testing.T) {
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
		},
		q,
	)
	ref := expressions.InitialOf(filter)

	rules := append(DefaultExpressionRules(), BatchAExpressionRules()...)
	p := NewPlanner(rules, ctx).WithImplementationRules(DefaultImplementationRules())
	if _, _, err := p.Plan(ref); err != nil {
		t.Fatalf("Plan: %v", err)
	}

	// After planning: look for a physicalIndexScanWrapper in ref
	// (the all-predicates-consumed case yields it directly into the
	// filter's Reference).
	var foundIndexScan bool
	for _, m := range ref.AllMembers() {
		if _, ok := m.(*physicalIndexScanWrapper); ok {
			foundIndexScan = true
			break
		}
	}
	if !foundIndexScan {
		t.Fatalf("planner did not produce an index scan wrapper; members=%d", len(ref.AllMembers()))
	}
}

func TestImplementIndexScanRule_PlannerIntegration_MultipleCandidates(t *testing.T) {
	t.Parallel()

	a1 := values.UniqueCorrelationIdentifier()
	b1 := values.UniqueCorrelationIdentifier()
	b2 := values.UniqueCorrelationIdentifier()
	cand1 := NewValueIndexScanMatchCandidate(
		"Order$status",
		[]string{"Order"},
		[]string{"STATUS"},
		[]values.CorrelationIdentifier{a1},
		values.UnknownType,
		false,
		nil,
	)
	cand2 := NewValueIndexScanMatchCandidate(
		"Order$status_date",
		[]string{"Order"},
		[]string{"STATUS", "DATE"},
		[]values.CorrelationIdentifier{b1, b2},
		values.UnknownType,
		false,
		nil,
	)
	ctx := &indexTestPlanContext{candidates: []MatchCandidate{cand1, cand2}}

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
				&values.FieldValue{Field: "DATE", Typ: values.TypeInt},
				predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(20260101)),
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

	// Both candidates should produce index scans. The 2-column index
	// (Order$status_date) subsumes both predicates and yields a bare
	// index scan; the 1-column index yields a filter-over-index-scan.
	indexScanCount := 0
	for _, m := range ref.AllMembers() {
		if _, ok := m.(*physicalIndexScanWrapper); ok {
			indexScanCount++
		}
	}
	if indexScanCount < 1 {
		t.Fatalf("expected at least 1 index scan wrapper, got %d", indexScanCount)
	}
}

type indexTestPlanContext struct {
	candidates []MatchCandidate
}

func (c *indexTestPlanContext) GetPlannerConfiguration() PlannerConfiguration {
	return DefaultPlannerConfiguration()
}

func (c *indexTestPlanContext) GetMatchCandidates() []MatchCandidate {
	return c.candidates
}

func (c *indexTestPlanContext) GetPrimaryKeyColumns(string) []string {
	return nil
}
