package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// --- Test helpers (local to this file) ---
//
// These mirror Java's RuleTestHelper / FDBQueryGraphTestHelpers.
// They are self-contained so the test file compiles independently
// of helpers that may or may not exist in other test files.

// pbFieldValue creates a FieldValue referencing a quantifier's field.
// Mirrors Java's fieldValue(Quantifier, String).
func pbFieldValue(q expressions.Quantifier, field string) *values.FieldValue {
	return values.NewFieldValue(q.GetFlowedObjectValue(), field, values.TypeUnknown)
}

// pbFieldPred creates a ComparisonPredicate on a quantifier's field.
// Mirrors Java's fieldPredicate(Quantifier, String, Comparison).
func pbFieldPred(q expressions.Quantifier, field string, cmp predicates.Comparison) *predicates.ComparisonPredicate {
	return &predicates.ComparisonPredicate{
		Operand:    pbFieldValue(q, field),
		Comparison: cmp,
	}
}

// pbValueCmp creates a ValueComparison (comparison where RHS is a Value).
func pbValueCmp(typ predicates.ComparisonType, v values.Value) predicates.Comparison {
	return predicates.Comparison{Type: typ, Operand: v}
}

// pbLiteralCmp creates a comparison with a literal operand.
func pbLiteralCmp(typ predicates.ComparisonType, lit any) predicates.Comparison {
	return predicates.NewLiteralComparison(typ, lit)
}

// pbForEachOf wraps an expression in a ForEach quantifier.
func pbForEachOf(expr expressions.RelationalExpression) expressions.Quantifier {
	return expressions.ForEachQuantifier(expressions.InitialOf(expr))
}

// pbSelectWithPreds creates a SelectExpression with one quantifier and
// the given predicates, using qun.GetFlowedObjectValue() as the result value.
func pbSelectWithPreds(qun expressions.Quantifier, preds ...predicates.QueryPredicate) *expressions.SelectExpression {
	return expressions.NewSelectExpression(
		qun.GetFlowedObjectValue(),
		[]expressions.Quantifier{qun},
		preds,
	)
}

// baseT mirrors Java's RuleTestHelper.baseT(): LogicalTypeFilter("T") over
// FullUnorderedScan, wrapped in a ForEach quantifier.
func baseT() expressions.Quantifier {
	scan := &expressions.FullUnorderedScanExpression{}
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)
	tf := expressions.NewLogicalTypeFilterExpression([]string{"T"}, scanQ)
	return pbForEachOf(tf)
}

// baseTau mirrors Java's RuleTestHelper.baseTau(): LogicalTypeFilter("TAU")
// over FullUnorderedScan, wrapped in a ForEach quantifier.
func baseTau() expressions.Quantifier {
	scan := &expressions.FullUnorderedScanExpression{}
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)
	tf := expressions.NewLogicalTypeFilterExpression([]string{"TAU"}, scanQ)
	return pbForEachOf(tf)
}

// explodeFieldQ mirrors Java's RuleTestHelper.explodeField(qun, "fieldName"):
// ForEach(ExplodeExpression(fieldValue(qun, fieldName))).
func explodeFieldQ(qun expressions.Quantifier, fieldName string) expressions.Quantifier {
	return pbForEachOf(expressions.NewExplodeExpression(pbFieldValue(qun, fieldName)))
}

// joinBuilder accumulates quantifiers, result columns, and predicates
// for constructing a SelectExpression via GraphExpansionBuilder.
type joinBuilder struct {
	gb *GraphExpansionBuilder
}

// joinOf mirrors Java's RuleTestHelper.join(quns...).
func joinOf(quns ...expressions.Quantifier) *joinBuilder {
	gb := NewGraphExpansionBuilder()
	for _, q := range quns {
		gb.AddQuantifier(q)
	}
	return &joinBuilder{gb: gb}
}

// addResultColumn mirrors Java's .addResultColumn(projectColumn(qun, "name")),
// which projects qun.fieldName AS name.
func (b *joinBuilder) addResultColumn(qun expressions.Quantifier, name string) *joinBuilder {
	b.gb.AddColumn(name, pbFieldValue(qun, name))
	return b
}

// addPredicate mirrors Java's .addPredicate(pred).
func (b *joinBuilder) addPredicate(pred predicates.QueryPredicate) *joinBuilder {
	b.gb.AddPredicate(pred)
	return b
}

// buildSelect seals the expansion and builds the SelectExpression.
func (b *joinBuilder) buildSelect() *expressions.SelectExpression {
	return b.gb.Build().Seal().BuildSelect()
}

// --- Uncorrelated join tests ---

func TestPartitionBinarySelectRule_PartitionSimpleSelect(t *testing.T) {
	t.Parallel()

	// A binary select with each predicate isolated to one side.
	// Predicates should be pushed to the appropriate side.
	//
	// Ports Java's PartitionBinarySelectRuleTest.partitionSimpleSelect.

	tQ := baseT()
	tauQ := baseTau()

	sel := joinOf(tQ, tauQ).
		addResultColumn(tQ, "a").
		addResultColumn(tauQ, "alpha").
		addPredicate(pbFieldPred(tQ, "b", pbLiteralCmp(predicates.ComparisonGreaterThan, "hello"))).
		addPredicate(pbFieldPred(tauQ, "beta", pbLiteralCmp(predicates.ComparisonGreaterThan, "world"))).
		buildSelect()

	ref := expressions.InitialOf(sel)
	yielded := FireExpressionRule(NewPartitionBinarySelectRule(), ref)

	// Both orderings are explored (no correlation between the two
	// independent quantifiers). Each ordering pushes predicates to
	// their respective sides. Expect 2 yields.
	if len(yielded) < 1 {
		t.Fatalf("expected at least 1 yielded expression, got %d", len(yielded))
	}

	for i, y := range yielded {
		result, ok := y.(*expressions.SelectExpression)
		if !ok {
			t.Fatalf("yield[%d]: expected *SelectExpression, got %T", i, y)
		}

		// Outer select should have 2 quantifiers and NO predicates
		// (all predicates absorbed into sub-selects).
		qs := result.GetQuantifiers()
		if len(qs) != 2 {
			t.Errorf("yield[%d]: expected 2 quantifiers, got %d", i, len(qs))
		}
		if result.HasPredicates() {
			t.Errorf("yield[%d]: outer select should have no predicates, got %d", i, len(result.GetPredicates()))
		}

		// Each sub-select should have exactly 1 predicate.
		for j, q := range qs {
			subRef := q.GetRangesOver()
			subExpr := subRef.Get()
			subSel, ok := subExpr.(*expressions.SelectExpression)
			if !ok {
				t.Errorf("yield[%d] qun[%d]: expected *SelectExpression child, got %T", i, j, subExpr)
				continue
			}
			if len(subSel.GetPredicates()) != 1 {
				t.Errorf("yield[%d] qun[%d]: expected 1 predicate in sub-select, got %d", i, j, len(subSel.GetPredicates()))
			}
		}
	}
}

func TestPartitionBinarySelectRule_PushJoinCriterionBothSides(t *testing.T) {
	t.Parallel()

	// A binary select with one join predicate (references both sides).
	// The rule should create candidates pushing the join predicate
	// to each side in turn.
	//
	// Ports Java's PartitionBinarySelectRuleTest.pushSimpleJoinCriterionBothSides.

	tQ := baseT()
	tauQ := baseTau()
	joinPred := pbFieldPred(tQ, "b",
		pbValueCmp(predicates.ComparisonEquals, pbFieldValue(tauQ, "beta")))

	sel := joinOf(tQ, tauQ).
		addResultColumn(tQ, "a").
		addResultColumn(tauQ, "alpha").
		addPredicate(joinPred).
		buildSelect()

	ref := expressions.InitialOf(sel)
	yielded := FireExpressionRule(NewPartitionBinarySelectRule(), ref)

	// For each ordering, the join predicate references both aliases.
	// It goes to the "right" side (the one whose alias appears in the
	// predicate's correlation set). Two orderings -> 2 yields.
	if len(yielded) < 2 {
		t.Fatalf("expected at least 2 yielded expressions, got %d", len(yielded))
	}

	for i, y := range yielded {
		result := y.(*expressions.SelectExpression)
		qs := result.GetQuantifiers()
		if len(qs) != 2 {
			t.Errorf("yield[%d]: expected 2 quantifiers, got %d", i, len(qs))
		}
		// Outer has no predicates.
		if result.HasPredicates() {
			t.Errorf("yield[%d]: outer should have no predicates", i)
		}

		// Exactly one of the two sub-quantifiers should have the join
		// predicate pushed into it (the other stays as the original).
		pushedCount := 0
		for _, q := range qs {
			sub := q.GetRangesOver().Get()
			if subSel, ok := sub.(*expressions.SelectExpression); ok {
				if len(subSel.GetPredicates()) > 0 {
					pushedCount++
				}
			}
		}
		if pushedCount != 1 {
			t.Errorf("yield[%d]: expected exactly 1 quantifier with pushed predicates, got %d", i, pushedCount)
		}
	}
}

func TestPartitionBinarySelectRule_PushUncorrelatedPredicateOppositeToJoin(t *testing.T) {
	t.Parallel()

	// A binary select with a join predicate and an uncorrelated predicate
	// (references neither side). The uncorrelated predicate should go to
	// the "left" side (independent of join), while the join predicate
	// goes to the "right" side.
	//
	// Ports Java's PartitionBinarySelectRuleTest.pushUncorrelatedPredicateOppositeToJoin.

	tQ := baseT()
	tauQ := baseTau()
	joinPred := pbFieldPred(tQ, "b",
		pbValueCmp(predicates.ComparisonEquals, pbFieldValue(tauQ, "beta")))

	// Uncorrelated predicate: 42 = <constant>. References no quantifier alias.
	uncorrelatedPred := &predicates.ComparisonPredicate{
		Operand:    values.LiteralValue(int64(42)),
		Comparison: predicates.Comparison{Type: predicates.ComparisonEquals, Operand: values.LiteralValue(int64(99))},
	}

	sel := joinOf(tQ, tauQ).
		addResultColumn(tQ, "a").
		addResultColumn(tauQ, "alpha").
		addPredicate(joinPred).
		addPredicate(uncorrelatedPred).
		buildSelect()

	ref := expressions.InitialOf(sel)
	yielded := FireExpressionRule(NewPartitionBinarySelectRule(), ref)

	// Two orderings -> 2 yields. Each yield has both predicates pushed
	// into sub-selects, leaving the outer empty.
	if len(yielded) < 2 {
		t.Fatalf("expected at least 2 yielded expressions, got %d", len(yielded))
	}

	for i, y := range yielded {
		result := y.(*expressions.SelectExpression)
		if result.HasPredicates() {
			t.Errorf("yield[%d]: outer should have no predicates", i)
		}
		qs := result.GetQuantifiers()
		if len(qs) != 2 {
			t.Errorf("yield[%d]: expected 2 quantifiers, got %d", i, len(qs))
			continue
		}
		// Each sub-select should have exactly 1 predicate: one gets the
		// uncorrelated, the other gets the join predicate.
		totalPreds := 0
		for _, q := range qs {
			sub := q.GetRangesOver().Get()
			if subSel, ok := sub.(*expressions.SelectExpression); ok {
				totalPreds += len(subSel.GetPredicates())
			}
		}
		if totalPreds != 2 {
			t.Errorf("yield[%d]: expected 2 total predicates across sub-selects, got %d", i, totalPreds)
		}
	}
}

func TestPartitionBinarySelectRule_PartitionWhenOneSideNotInResultValue(t *testing.T) {
	t.Parallel()

	// Partition a binary join when one side does not appear in the result value.
	// The side that does not appear effectively only filters out results.
	//
	// Ports Java's PartitionBinarySelectRuleTest.partitionWhenOneSideNotInResultValue.

	tQ := baseT()
	tauQ := baseTau()
	joinPred := pbFieldPred(tQ, "b",
		pbValueCmp(predicates.ComparisonEquals, pbFieldValue(tauQ, "beta")))

	sel := joinOf(tQ, tauQ).
		addResultColumn(tQ, "a"). // only t in result value
		addPredicate(joinPred).
		addPredicate(pbFieldPred(tQ, "c", predicates.Comparison{Type: predicates.ComparisonIsNotNull})).
		addPredicate(pbFieldPred(tauQ, "gamma", predicates.Comparison{Type: predicates.ComparisonIsNotNull})).
		buildSelect()

	ref := expressions.InitialOf(sel)
	yielded := FireExpressionRule(NewPartitionBinarySelectRule(), ref)

	if len(yielded) < 2 {
		t.Fatalf("expected at least 2 yielded expressions, got %d", len(yielded))
	}

	for i, y := range yielded {
		result := y.(*expressions.SelectExpression)
		if result.HasPredicates() {
			t.Errorf("yield[%d]: outer should have no predicates", i)
		}
		qs := result.GetQuantifiers()
		if len(qs) != 2 {
			t.Errorf("yield[%d]: expected 2 quantifiers, got %d", i, len(qs))
		}
	}
}

func TestPartitionBinarySelectRule_SkipsExistentialQuantifiers(t *testing.T) {
	t.Parallel()

	// Go's PartitionBinarySelectRule explicitly skips expressions with
	// existential quantifiers (see guard at lines 64-68 of the rule).
	// This covers Java's partitionUncorrelatedExistential,
	// partitionPredicatesWithExists, and
	// partitionPredicateWhereCorrelationComesBelowExists -- all of which
	// involve existential quantifiers and would fire in Java but not in Go.
	//
	// The Go rule will handle these in a future shift when Memo-level
	// dedup prevents the aliasing issues documented in the rule source.

	tQ := baseT()
	tauLower := baseTau()
	tauExistsRef := expressions.InitialOf(
		pbSelectWithPreds(tauLower,
			pbFieldPred(tauLower, "beta", predicates.Comparison{Type: predicates.ComparisonIsNull})),
	)
	tauQ := expressions.ExistentialQuantifier(tauExistsRef)

	sel := expressions.NewSelectExpression(
		tQ.GetFlowedObjectValue(),
		[]expressions.Quantifier{tQ, tauQ},
		[]predicates.QueryPredicate{
			predicates.NewExistentialAlias(tauQ.GetAlias()),
			pbFieldPred(tQ, "c", predicates.Comparison{Type: predicates.ComparisonIsNotNull}),
		},
	)
	ref := expressions.InitialOf(sel)

	yielded := FireExpressionRule(NewPartitionBinarySelectRule(), ref)
	if len(yielded) != 0 {
		t.Fatalf("expected 0 yields (existential quantifiers are skipped by Go rule), got %d", len(yielded))
	}
}

// --- Correlated join tests ---
//
// These use ExplodeExpression which creates a correlation from the exploded
// quantifier back to the base. However, Go's Quantifier.GetCorrelatedTo()
// is currently stubbed (returns empty set), so computeTransitiveCorrelationOrder
// sees no dependencies. Both orderings will be attempted.

func TestPartitionBinarySelectRule_PushSimplePredicatesWithCorrelatedQuantifiers(t *testing.T) {
	t.Parallel()

	// Binary join: t and g (explode of t.g). Predicates on each side
	// should be pushed to the appropriate side.
	//
	// Ports Java's PartitionBinarySelectRuleTest.pushSimplePredicatesWithCorrelatedQuantifiers.
	//
	// NOTE: Java produces 1 yield (only the valid ordering because g is
	// correlated to t). Go produces yields for both orderings because
	// GetCorrelatedTo() is stubbed. We verify the structural properties
	// rather than exact count.

	tQ := baseT()
	gQ := explodeFieldQ(tQ, "g")

	sel := joinOf(tQ, gQ).
		addResultColumn(tQ, "a").
		addResultColumn(gQ, "one").
		addPredicate(pbFieldPred(tQ, "b", pbLiteralCmp(predicates.ComparisonEquals, "hello"))).
		addPredicate(pbFieldPred(gQ, "two", pbLiteralCmp(predicates.ComparisonEquals, "world"))).
		buildSelect()

	ref := expressions.InitialOf(sel)
	yielded := FireExpressionRule(NewPartitionBinarySelectRule(), ref)

	if len(yielded) < 1 {
		t.Fatalf("expected at least 1 yielded expression, got %d", len(yielded))
	}

	for i, y := range yielded {
		result := y.(*expressions.SelectExpression)
		// Outer should have no predicates.
		if result.HasPredicates() {
			t.Errorf("yield[%d]: outer should have no predicates, got %d", i, len(result.GetPredicates()))
		}
		qs := result.GetQuantifiers()
		if len(qs) != 2 {
			t.Errorf("yield[%d]: expected 2 quantifiers, got %d", i, len(qs))
		}
	}
}

func TestPartitionBinarySelectRule_PushCorrelatedPredicate(t *testing.T) {
	t.Parallel()

	// Binary join: t and g (explode of t.g) with a join predicate
	// t.b = g.two. The predicate references both sides.
	//
	// Ports Java's PartitionBinarySelectRuleTest.pushCorrelatedPredicateWithQunCorrelation.
	//
	// NOTE: Java produces 1 yield (only pushes to g since t->g correlation
	// blocks the reverse). Go produces 2 yields because GetCorrelatedTo()
	// is stubbed.

	tQ := baseT()
	gQ := explodeFieldQ(tQ, "g")
	joinPred := pbFieldPred(tQ, "b",
		pbValueCmp(predicates.ComparisonEquals, pbFieldValue(gQ, "two")))

	sel := joinOf(tQ, gQ).
		addResultColumn(tQ, "a").
		addResultColumn(gQ, "one").
		addPredicate(joinPred).
		buildSelect()

	ref := expressions.InitialOf(sel)
	yielded := FireExpressionRule(NewPartitionBinarySelectRule(), ref)

	if len(yielded) < 1 {
		t.Fatalf("expected at least 1 yielded expression, got %d", len(yielded))
	}

	// In at least one yield, the join predicate should be pushed to one side.
	foundPushed := false
	for _, y := range yielded {
		result := y.(*expressions.SelectExpression)
		if result.HasPredicates() {
			continue // This yield didn't push all predicates
		}
		for _, q := range result.GetQuantifiers() {
			sub := q.GetRangesOver().Get()
			if subSel, ok := sub.(*expressions.SelectExpression); ok {
				if len(subSel.GetPredicates()) == 1 {
					foundPushed = true
				}
			}
		}
	}
	if !foundPushed {
		t.Error("expected at least one yield with the join predicate pushed into a sub-select")
	}
}

func TestPartitionBinarySelectRule_PartitionWithExplodeNotInResult(t *testing.T) {
	t.Parallel()

	// Partition when there's an explode and the exploded value does NOT
	// contribute to the final result value. Predicates should still be
	// pushed.
	//
	// Ports Java's PartitionBinarySelectRuleTest.partitionSelectWhenResultValueComesFromRecordWithExplode.

	tQ := baseT()
	gQ := explodeFieldQ(tQ, "g")
	joinPred := pbFieldPred(tQ, "b",
		pbValueCmp(predicates.ComparisonEquals, pbFieldValue(gQ, "two")))

	sel := joinOf(tQ, gQ).
		addResultColumn(tQ, "a"). // only t in result
		addPredicate(joinPred).
		addPredicate(pbFieldPred(tQ, "c", predicates.Comparison{Type: predicates.ComparisonIsNull})).
		addPredicate(pbFieldPred(gQ, "three", predicates.Comparison{Type: predicates.ComparisonIsNull})).
		buildSelect()

	ref := expressions.InitialOf(sel)
	yielded := FireExpressionRule(NewPartitionBinarySelectRule(), ref)

	if len(yielded) < 1 {
		t.Fatalf("expected at least 1 yielded expression, got %d", len(yielded))
	}

	for i, y := range yielded {
		result := y.(*expressions.SelectExpression)
		// All 3 predicates should be absorbed into sub-selects.
		if result.HasPredicates() {
			t.Errorf("yield[%d]: outer should have no predicates, got %d", i, len(result.GetPredicates()))
		}
		qs := result.GetQuantifiers()
		if len(qs) != 2 {
			t.Errorf("yield[%d]: expected 2 quantifiers, got %d", i, len(qs))
			continue
		}

		// Count total predicates across sub-selects.
		totalPreds := 0
		for _, q := range qs {
			sub := q.GetRangesOver().Get()
			if subSel, ok := sub.(*expressions.SelectExpression); ok {
				totalPreds += len(subSel.GetPredicates())
			}
		}
		if totalPreds != 3 {
			t.Errorf("yield[%d]: expected 3 total predicates across sub-selects, got %d", i, totalPreds)
		}
	}
}

func TestPartitionBinarySelectRule_PartitionWithExplodeInResult(t *testing.T) {
	t.Parallel()

	// Partition when the exploded value IS the result value. The record
	// side (t) is not in the result value but still gets its predicate
	// pushed down.
	//
	// Ports Java's PartitionBinarySelectRuleTest.partitionSelectWhenResultValueComesFromExplodedField.

	tQ := baseT()
	gQ := explodeFieldQ(tQ, "g")

	sel := joinOf(tQ, gQ).
		addResultColumn(gQ, "one"). // only exploded side in result
		addPredicate(pbFieldPred(tQ, "b", pbLiteralCmp(predicates.ComparisonLessThan, "hello"))).
		addPredicate(pbFieldPred(gQ, "two", pbLiteralCmp(predicates.ComparisonLessThan, "world"))).
		buildSelect()

	ref := expressions.InitialOf(sel)
	yielded := FireExpressionRule(NewPartitionBinarySelectRule(), ref)

	if len(yielded) < 1 {
		t.Fatalf("expected at least 1 yielded expression, got %d", len(yielded))
	}

	for i, y := range yielded {
		result := y.(*expressions.SelectExpression)
		// Outer should have no predicates.
		if result.HasPredicates() {
			t.Errorf("yield[%d]: outer should have no predicates, got %d", i, len(result.GetPredicates()))
		}
		qs := result.GetQuantifiers()
		if len(qs) != 2 {
			t.Errorf("yield[%d]: expected 2 quantifiers, got %d", i, len(qs))
		}
	}
}

func TestPartitionBinarySelectRule_NoPredicates(t *testing.T) {
	t.Parallel()

	// A binary select with NO predicates at all. Nothing to push.
	// The rule should yield nothing.

	tQ := baseT()
	tauQ := baseTau()

	sel := joinOf(tQ, tauQ).
		addResultColumn(tQ, "a").
		addResultColumn(tauQ, "alpha").
		buildSelect()

	ref := expressions.InitialOf(sel)
	yielded := FireExpressionRule(NewPartitionBinarySelectRule(), ref)

	if len(yielded) != 0 {
		t.Fatalf("expected 0 yields (no predicates to push), got %d", len(yielded))
	}
}

func TestPartitionBinarySelectRule_SingleQuantifier(t *testing.T) {
	t.Parallel()

	// A select with only one quantifier. The rule should not fire.

	tQ := baseT()

	sel := expressions.NewSelectExpression(
		tQ.GetFlowedObjectValue(),
		[]expressions.Quantifier{tQ},
		[]predicates.QueryPredicate{
			pbFieldPred(tQ, "a", pbLiteralCmp(predicates.ComparisonEquals, int64(42))),
		},
	)
	ref := expressions.InitialOf(sel)

	yielded := FireExpressionRule(NewPartitionBinarySelectRule(), ref)
	if len(yielded) != 0 {
		t.Fatalf("expected 0 yields (single quantifier), got %d", len(yielded))
	}
}

func TestPartitionBinarySelectRule_ThreeQuantifiers(t *testing.T) {
	t.Parallel()

	// A select with three quantifiers. The binary rule should not fire
	// (it requires exactly 2).

	tQ := baseT()
	tauQ := baseTau()
	extraQ := baseT()

	sel := expressions.NewSelectExpression(
		tQ.GetFlowedObjectValue(),
		[]expressions.Quantifier{tQ, tauQ, extraQ},
		[]predicates.QueryPredicate{
			pbFieldPred(tQ, "a", pbLiteralCmp(predicates.ComparisonEquals, int64(1))),
		},
	)
	ref := expressions.InitialOf(sel)

	yielded := FireExpressionRule(NewPartitionBinarySelectRule(), ref)
	if len(yielded) != 0 {
		t.Fatalf("expected 0 yields (three quantifiers, rule requires exactly 2), got %d", len(yielded))
	}
}

func TestPartitionBinarySelectRule_PredicateOnOneSideOnly(t *testing.T) {
	t.Parallel()

	// Only one side has a predicate. The rule should push it to that
	// side, leaving the other as-is.

	tQ := baseT()
	tauQ := baseTau()

	sel := joinOf(tQ, tauQ).
		addResultColumn(tQ, "a").
		addResultColumn(tauQ, "alpha").
		addPredicate(pbFieldPred(tQ, "b", pbLiteralCmp(predicates.ComparisonEquals, "hello"))).
		buildSelect()

	ref := expressions.InitialOf(sel)
	yielded := FireExpressionRule(NewPartitionBinarySelectRule(), ref)

	if len(yielded) < 1 {
		t.Fatalf("expected at least 1 yielded expression, got %d", len(yielded))
	}

	for i, y := range yielded {
		result := y.(*expressions.SelectExpression)
		if result.HasPredicates() {
			t.Errorf("yield[%d]: outer should have no predicates", i)
		}
		qs := result.GetQuantifiers()
		if len(qs) != 2 {
			t.Errorf("yield[%d]: expected 2 quantifiers, got %d", i, len(qs))
			continue
		}

		// Exactly one sub-select should have the predicate.
		pushCount := 0
		for _, q := range qs {
			sub := q.GetRangesOver().Get()
			if subSel, ok := sub.(*expressions.SelectExpression); ok {
				if len(subSel.GetPredicates()) == 1 {
					pushCount++
				}
			}
		}
		if pushCount != 1 {
			t.Errorf("yield[%d]: expected exactly 1 sub-select with a predicate, got %d", i, pushCount)
		}
	}
}

func TestPartitionBinarySelectRule_IdempotencyGuard(t *testing.T) {
	t.Parallel()

	// The idempotency guard fires only when the Reference already contains the
	// predicate-less partition of THIS select — a 2-quantifier predicate-free
	// SelectExpression over the SAME quantifier alias set. This is the rule's
	// OWN re-fire (it reuses the source quantifier aliases when it pushes
	// predicates into sub-Selects), so re-creating it is the cycle to break.
	tQ := baseT()
	tauQ := baseTau()

	// First select: has predicates.
	sel := joinOf(tQ, tauQ).
		addResultColumn(tQ, "a").
		addResultColumn(tauQ, "alpha").
		addPredicate(pbFieldPred(tQ, "b", pbLiteralCmp(predicates.ComparisonEquals, "hello"))).
		buildSelect()

	// Prior-fire result: no predicates, SAME quantifier aliases (tQ, tauQ).
	noopSel := joinOf(tQ, tauQ).
		addResultColumn(tQ, "a").
		addResultColumn(tauQ, "alpha").
		buildSelect()

	ref := expressions.InitialOf(sel)
	ref.Insert(noopSel)

	yielded := FireExpressionRule(NewPartitionBinarySelectRule(), ref)
	if len(yielded) != 0 {
		t.Fatalf("expected 0 yields (idempotency guard: predicate-free partition over the same alias set already present), got %d", len(yielded))
	}
}

// TestPartitionBinarySelectRule_DistinctBipartitionNotBlocked pins the narrowed
// guard's load-bearing dimension: a predicate-free 2-quantifier select over a
// DIFFERENT quantifier alias set is a sibling bipartition of the same join (e.g.
// {$m(t1⋈t2), t3} vs {$m(t2⋈t3), t1}), NOT this select's own re-fire. The earlier
// "any predicate-free binary in the group" guard blocked it, so the correlated
// index-probe FlatMap chain for ≥3-way joins was never enumerated (the inner
// materialized as a full-scan NLJ). The guard must NOT block here.
func TestPartitionBinarySelectRule_DistinctBipartitionNotBlocked(t *testing.T) {
	t.Parallel()

	tQ := baseT()
	tauQ := baseTau()

	sel := joinOf(tQ, tauQ).
		addResultColumn(tQ, "a").
		addResultColumn(tauQ, "alpha").
		addPredicate(pbFieldPred(tQ, "b", pbLiteralCmp(predicates.ComparisonEquals, "hello"))).
		buildSelect()

	// A different bipartition's predicate-free binary: fresh, distinct aliases.
	tQ2 := baseT()
	tauQ2 := baseTau()
	otherSel := joinOf(tQ2, tauQ2).
		addResultColumn(tQ2, "a").
		addResultColumn(tauQ2, "alpha").
		buildSelect()

	ref := expressions.InitialOf(sel)
	ref.Insert(otherSel)

	yielded := FireExpressionRule(NewPartitionBinarySelectRule(), ref)
	if len(yielded) == 0 {
		t.Fatal("expected >0 yields: a predicate-free binary over a DIFFERENT alias set is a sibling bipartition, not this select's own re-fire — the guard must not block it")
	}
}

func TestPartitionBinarySelectRule_ResultValuePreserved(t *testing.T) {
	t.Parallel()

	// Verify that the result value from the original select is
	// preserved in the yielded outer select.

	tQ := baseT()
	tauQ := baseTau()

	sel := joinOf(tQ, tauQ).
		addResultColumn(tQ, "a").
		addResultColumn(tauQ, "alpha").
		addPredicate(pbFieldPred(tQ, "b", pbLiteralCmp(predicates.ComparisonEquals, "hello"))).
		buildSelect()

	originalResultValue := sel.GetResultValue()
	ref := expressions.InitialOf(sel)
	yielded := FireExpressionRule(NewPartitionBinarySelectRule(), ref)

	if len(yielded) < 1 {
		t.Fatalf("expected at least 1 yielded expression, got %d", len(yielded))
	}

	// The outer select's result value should be the same as the original's.
	result := yielded[0].(*expressions.SelectExpression)
	rv := result.GetResultValue()
	if rv == nil {
		t.Fatal("result value should not be nil")
	}

	// The result value should be structurally the same type (RecordConstructorValue)
	// as the original.
	_, origIsRCV := originalResultValue.(*values.RecordConstructorValue)
	_, resultIsRCV := rv.(*values.RecordConstructorValue)
	if origIsRCV != resultIsRCV {
		t.Errorf("result value type mismatch: original is RCV=%v, result is RCV=%v", origIsRCV, resultIsRCV)
	}
}
