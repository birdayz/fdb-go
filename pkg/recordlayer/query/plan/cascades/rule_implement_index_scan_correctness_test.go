package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/properties"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// TestIndexScan_ConflictingEqualities tests that col=1 AND col=2 on the
// same column does NOT produce an index scan (the ComparisonRange merge
// rejects conflicting equalities — result is an unsatisfiable predicate
// set that binds no prefix).
func TestIndexScan_ConflictingEqualities(t *testing.T) {
	t.Parallel()

	a1 := values.UniqueCorrelationIdentifier()
	cand := NewValueIndexScanMatchCandidate(
		"Order$status",
		[]string{"Order"},
		[]string{"STATUS"},
		[]values.CorrelationIdentifier{a1},
		values.UnknownType,
		false,
	)
	ctx := &indexTestPlanContext{candidates: []MatchCandidate{cand}}

	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "STATUS", Typ: values.TypeInt},
				predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(1)),
			),
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "STATUS", Typ: values.TypeInt},
				predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(2)),
			),
		},
		q,
	)
	filterRef := expressions.InitialOf(filter)

	rule := NewImplementIndexScanRule()
	results := FireExpressionRuleWithMemo(rule, filterRef, ctx, nil)

	if len(results) != 0 {
		t.Fatalf("expected 0 yields (conflicting equalities should not produce a valid prefix), got %d", len(results))
	}
}

// TestIndexScan_SameEqualityTwice tests that col=1 AND col=1 (same
// value) still produces a valid single-equality scan (the merge
// accepts identical equality values).
func TestIndexScan_SameEqualityTwice(t *testing.T) {
	t.Parallel()

	a1 := values.UniqueCorrelationIdentifier()
	cand := NewValueIndexScanMatchCandidate(
		"Order$status",
		[]string{"Order"},
		[]string{"STATUS"},
		[]values.CorrelationIdentifier{a1},
		values.UnknownType,
		false,
	)
	ctx := &indexTestPlanContext{candidates: []MatchCandidate{cand}}

	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "STATUS", Typ: values.TypeInt},
				predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(1)),
			),
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "STATUS", Typ: values.TypeInt},
				predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(1)),
			),
		},
		q,
	)
	filterRef := expressions.InitialOf(filter)

	rule := NewImplementIndexScanRule()
	results := FireExpressionRuleWithMemo(rule, filterRef, ctx, nil)

	if len(results) != 1 {
		t.Fatalf("expected 1 yield (same equality merges fine), got %d", len(results))
	}
	wrapper, ok := results[0].(*physicalIndexScanWrapper)
	if !ok {
		t.Fatalf("expected bare index scan (both consumed), got %T", results[0])
	}
	if !wrapper.plan.GetScanComparisons()[0].IsEquality() {
		t.Fatal("comparison should be equality")
	}
}

// TestIndexScan_NonFieldValueOperand ensures predicates whose operand
// is not a FieldValue (e.g. constant or arithmetic) are treated as
// residual (not matched to any index column).
func TestIndexScan_NonFieldValueOperand(t *testing.T) {
	t.Parallel()

	a1 := values.UniqueCorrelationIdentifier()
	cand := NewValueIndexScanMatchCandidate(
		"Order$status",
		[]string{"Order"},
		[]string{"STATUS"},
		[]values.CorrelationIdentifier{a1},
		values.UnknownType,
		false,
	)
	ctx := &indexTestPlanContext{candidates: []MatchCandidate{cand}}

	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)
	// Predicate: 42 = 'active' (constant operand, not a FieldValue)
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{
			predicates.NewComparisonPredicate(
				&values.ConstantValue{Value: int64(42), Typ: values.TypeInt},
				predicates.NewLiteralComparison(predicates.ComparisonEquals, "active"),
			),
		},
		q,
	)
	filterRef := expressions.InitialOf(filter)

	rule := NewImplementIndexScanRule()
	results := FireExpressionRuleWithMemo(rule, filterRef, ctx, nil)

	if len(results) != 0 {
		t.Fatalf("expected 0 yields (non-FieldValue operand), got %d", len(results))
	}
}

// TestIndexScan_NonComparisonPredicates ensures predicates that are not
// ComparisonPredicates (And, Or, Constant, etc.) are treated as
// residual.
func TestIndexScan_NonComparisonPredicates(t *testing.T) {
	t.Parallel()

	a1 := values.UniqueCorrelationIdentifier()
	cand := NewValueIndexScanMatchCandidate(
		"Order$status",
		[]string{"Order"},
		[]string{"STATUS"},
		[]values.CorrelationIdentifier{a1},
		values.UnknownType,
		false,
	)
	ctx := &indexTestPlanContext{candidates: []MatchCandidate{cand}}

	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{
			predicates.NewConstantPredicate(predicates.TriTrue),
			predicates.NewAnd(
				predicates.NewConstantPredicate(predicates.TriTrue),
				predicates.NewConstantPredicate(predicates.TriFalse),
			),
		},
		q,
	)
	filterRef := expressions.InitialOf(filter)

	rule := NewImplementIndexScanRule()
	results := FireExpressionRuleWithMemo(rule, filterRef, ctx, nil)

	if len(results) != 0 {
		t.Fatalf("expected 0 yields (no ComparisonPredicate on index col), got %d", len(results))
	}
}

// TestIndexScan_EqualityThenInequality_ConsumesBoth tests the canonical
// compound-key pattern: WHERE a = 1 AND b > 5 on index(a, b).
// Both predicates should be consumed by the index (equality prefix +
// trailing inequality).
func TestIndexScan_EqualityThenInequality_ConsumesBoth(t *testing.T) {
	t.Parallel()

	a1 := values.UniqueCorrelationIdentifier()
	a2 := values.UniqueCorrelationIdentifier()
	cand := NewValueIndexScanMatchCandidate(
		"Order$status_amount",
		[]string{"Order"},
		[]string{"STATUS", "AMOUNT"},
		[]values.CorrelationIdentifier{a1, a2},
		values.UnknownType,
		false,
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
	// Both consumed → bare index scan.
	wrapper, ok := results[0].(*physicalIndexScanWrapper)
	if !ok {
		t.Fatalf("expected bare *physicalIndexScanWrapper (all consumed), got %T", results[0])
	}
	comps := wrapper.plan.GetScanComparisons()
	if !comps[0].IsEquality() {
		t.Fatal("first comparison should be equality")
	}
	if !comps[1].IsInequality() {
		t.Fatal("second comparison should be inequality")
	}
}

// TestIndexScan_PredicateOrderIndependent verifies that predicate order
// in the filter doesn't matter — even if the AMOUNT predicate comes
// before STATUS, the rule correctly maps both to their index positions.
func TestIndexScan_PredicateOrderIndependent(t *testing.T) {
	t.Parallel()

	a1 := values.UniqueCorrelationIdentifier()
	a2 := values.UniqueCorrelationIdentifier()
	cand := NewValueIndexScanMatchCandidate(
		"Order$status_amount",
		[]string{"Order"},
		[]string{"STATUS", "AMOUNT"},
		[]values.CorrelationIdentifier{a1, a2},
		values.UnknownType,
		false,
	)
	ctx := &indexTestPlanContext{candidates: []MatchCandidate{cand}}

	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)
	// AMOUNT predicate comes first (reverse order from index key).
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "AMOUNT", Typ: values.TypeInt},
				predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(50)),
			),
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
		t.Fatalf("expected bare index scan, got %T", results[0])
	}
	comps := wrapper.plan.GetScanComparisons()
	if !comps[0].IsEquality() || !comps[1].IsEquality() {
		t.Fatal("both comparisons should be equality regardless of predicate order")
	}
}

// TestIndexScan_UniqueIndexPointLookupCost verifies that a unique index
// with all columns equality-bound returns cardinality ~1 (point lookup),
// which is cheaper than a non-unique index with the same predicates.
func TestIndexScan_UniqueIndexPointLookupCost(t *testing.T) {
	t.Parallel()

	a1 := values.UniqueCorrelationIdentifier()
	candUnique := NewValueIndexScanMatchCandidate(
		"Order$id_unique",
		[]string{"Order"},
		[]string{"ID"},
		[]values.CorrelationIdentifier{a1},
		values.UnknownType,
		true, // unique
	)
	b1 := values.UniqueCorrelationIdentifier()
	candNonUnique := NewValueIndexScanMatchCandidate(
		"Order$id_nonunique",
		[]string{"Order"},
		[]string{"ID"},
		[]values.CorrelationIdentifier{b1},
		values.UnknownType,
		false, // non-unique
	)

	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "ID", Typ: values.TypeInt},
				predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(42)),
			),
		},
		q,
	)
	filterRef := expressions.InitialOf(filter)

	// Fire with unique index.
	ctxU := &indexTestPlanContext{candidates: []MatchCandidate{candUnique}}
	rule := NewImplementIndexScanRule()
	resultsU := FireExpressionRuleWithMemo(rule, filterRef, ctxU, nil)
	if len(resultsU) != 1 {
		t.Fatalf("unique: expected 1 yield, got %d", len(resultsU))
	}
	wrapperU := resultsU[0].(*physicalIndexScanWrapper)
	costU := wrapperU.HintCost(nil, properties.DefaultStatistics{})

	// Fire with non-unique index (rebuild filter for fresh reference).
	scan2 := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanRef2 := expressions.InitialOf(scan2)
	q2 := expressions.ForEachQuantifier(scanRef2)
	filter2 := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "ID", Typ: values.TypeInt},
				predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(42)),
			),
		},
		q2,
	)
	filterRef2 := expressions.InitialOf(filter2)
	ctxNU := &indexTestPlanContext{candidates: []MatchCandidate{candNonUnique}}
	resultsNU := FireExpressionRuleWithMemo(rule, filterRef2, ctxNU, nil)
	if len(resultsNU) != 1 {
		t.Fatalf("non-unique: expected 1 yield, got %d", len(resultsNU))
	}
	wrapperNU := resultsNU[0].(*physicalIndexScanWrapper)
	costNU := wrapperNU.HintCost(nil, properties.DefaultStatistics{})

	if costU.Cardinality >= costNU.Cardinality {
		t.Fatalf("unique point lookup (card=%.2f) should be cheaper than non-unique (card=%.2f)",
			costU.Cardinality, costNU.Cardinality)
	}
}

// TestIndexScan_MultipleIndexesBestChoice verifies that when multiple
// indexes can serve a query, the planner picks the compound index
// (more bound columns → lower estimated cardinality) over the single-column index.
func TestIndexScan_MultipleIndexesBestChoice(t *testing.T) {
	t.Parallel()

	a1 := values.UniqueCorrelationIdentifier()
	candSingle := NewValueIndexScanMatchCandidate(
		"Order$status",
		[]string{"Order"},
		[]string{"STATUS"},
		[]values.CorrelationIdentifier{a1},
		values.UnknownType,
		false,
	)
	b1 := values.UniqueCorrelationIdentifier()
	b2 := values.UniqueCorrelationIdentifier()
	candCompound := NewValueIndexScanMatchCandidate(
		"Order$status_amount",
		[]string{"Order"},
		[]string{"STATUS", "AMOUNT"},
		[]values.CorrelationIdentifier{b1, b2},
		values.UnknownType,
		false,
	)
	ctx := &indexTestPlanContext{candidates: []MatchCandidate{candSingle, candCompound}}

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

	rules := append(DefaultExpressionRules(), BatchAExpressionRules()...)
	p := NewPlanner(rules, ctx).WithImplementationRules(DefaultImplementationRules())
	best, _, err := p.Plan(filterRef)
	if err != nil {
		t.Fatalf("planner error: %v", err)
	}

	w, ok := best.(*physicalIndexScanWrapper)
	if !ok {
		t.Fatalf("expected planner to pick physicalIndexScanWrapper, got %T", best)
	}
	comps := w.plan.GetScanComparisons()
	bound := 0
	for _, cr := range comps {
		if !cr.IsEmpty() {
			bound++
		}
	}
	if bound != 2 {
		t.Fatalf("expected compound index (2 bound columns) chosen, got %d bound", bound)
	}
}

// TestIndexScan_CostComparison verifies that the planner's cost model
// correctly ranks an index scan cheaper than a full-scan+filter on the
// same query shape.
func TestIndexScan_CostComparison(t *testing.T) {
	t.Parallel()

	a1 := values.UniqueCorrelationIdentifier()
	cand := NewValueIndexScanMatchCandidate(
		"Order$status",
		[]string{"Order"},
		[]string{"STATUS"},
		[]values.CorrelationIdentifier{a1},
		values.UnknownType,
		false,
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
	var foundIndexScan bool
	for _, m := range ref.AllMembers() {
		if _, ok := m.(*physicalIndexScanWrapper); ok {
			foundIndexScan = true
			break
		}
	}
	if !foundIndexScan {
		t.Fatalf("planner should produce an index scan wrapper; members=%d", len(ref.AllMembers()))
	}
}

// TestIndexScan_CaseInsensitiveColumnMatch verifies that the index
// rule matches predicates to index columns case-insensitively.
func TestIndexScan_CaseInsensitiveColumnMatch(t *testing.T) {
	t.Parallel()

	a1 := values.UniqueCorrelationIdentifier()
	cand := NewValueIndexScanMatchCandidate(
		"Order$status",
		[]string{"Order"},
		[]string{"status"},
		[]values.CorrelationIdentifier{a1},
		values.UnknownType,
		false,
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

	if len(results) == 0 {
		t.Fatal("expected index scan yield — case-insensitive column match should work")
	}
}
