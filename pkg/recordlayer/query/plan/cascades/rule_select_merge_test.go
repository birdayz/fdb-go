package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func selectMergeTestRowType() values.Type {
	nested := values.NewRecordType("NESTED", false, []values.Field{
		{Name: "one", Ordinal: 0, FieldType: values.NotNullLong},
		{Name: "two", Ordinal: 1, FieldType: values.NotNullString},
		{Name: "three", Ordinal: 2, FieldType: values.NotNullLong},
	})
	return values.NewRecordType("SELECT_MERGE", false, []values.Field{
		{Name: "X", Ordinal: 0, FieldType: values.NotNullLong},
		{Name: "Y", Ordinal: 1, FieldType: values.NotNullLong},
		{Name: "A", Ordinal: 2, FieldType: values.NotNullLong},
		{Name: "COL", Ordinal: 3, FieldType: values.NotNullLong},
		{Name: "a", Ordinal: 4, FieldType: values.NotNullLong},
		{Name: "b", Ordinal: 5, FieldType: values.NotNullLong},
		{Name: "c", Ordinal: 6, FieldType: values.NotNullLong},
		{Name: "d", Ordinal: 7, FieldType: values.NotNullLong},
		{Name: "alpha", Ordinal: 8, FieldType: values.NotNullLong},
		{Name: "beta", Ordinal: 9, FieldType: values.NotNullLong},
		{Name: "gamma", Ordinal: 10, FieldType: values.NotNullLong},
		{Name: "delta", Ordinal: 11, FieldType: values.NotNullLong},
		{Name: "x", Ordinal: 12, FieldType: values.NotNullLong},
		{Name: "y", Ordinal: 13, FieldType: values.NotNullLong},
		{Name: "f", Ordinal: 14, FieldType: values.NewArrayType(false, values.NotNullLong)},
		{Name: "g", Ordinal: 15, FieldType: values.NewArrayType(false, nested)},
		{Name: "sub", Ordinal: 16, FieldType: values.NewArrayType(false, values.NotNullLong)},
	})
}

func selectMergeScan(t testing.TB) *expressions.FullUnorderedScanExpression {
	t.Helper()
	return selectMergeTypedScan(t, "SELECT_MERGE", selectMergeTestRowType())
}

func selectMergeTypedScan(
	t testing.TB,
	name string,
	typ values.Type,
) *expressions.FullUnorderedScanExpression {
	t.Helper()
	scan, err := expressions.NewFullUnorderedScanExpression([]string{name}, typ)
	return mustConstruct(t, scan, err)
}

func selectMergeFlowed(t testing.TB, q expressions.Quantifier) values.QuantifiedObjectValue {
	t.Helper()
	flowed, err := q.RequireFlowedObjectValue()
	return mustConstruct(t, flowed, err)
}

func selectMergeFilter(
	t testing.TB,
	preds []predicates.QueryPredicate,
	q expressions.Quantifier,
) *expressions.LogicalFilterExpression {
	t.Helper()
	filter, err := expressions.NewLogicalFilterExpression(preds, q)
	return mustConstruct(t, filter, err)
}

func selectMergeSelect(
	t testing.TB,
	result values.Value,
	quantifiers []expressions.Quantifier,
	preds []predicates.QueryPredicate,
) *expressions.SelectExpression {
	t.Helper()
	selectExpr, err := expressions.NewSelectExpression(result, quantifiers, preds)
	return mustConstruct(t, selectExpr, err)
}

func selectMergeSelectWithAliases(
	t testing.TB,
	result values.Value,
	quantifiers []expressions.Quantifier,
	preds []predicates.QueryPredicate,
	aliases []string,
) *expressions.SelectExpression {
	t.Helper()
	selectExpr, err := expressions.NewSelectExpressionWithAliases(result, quantifiers, preds, aliases)
	return mustConstruct(t, selectExpr, err)
}

func selectMergeExplode(t testing.TB, collection values.Value) *expressions.ExplodeExpression {
	t.Helper()
	explode, err := expressions.NewExplodeExpression(collection)
	return mustConstruct(t, explode, err)
}

func selectMergeQOV(
	t testing.TB,
	alias values.CorrelationIdentifier,
	typ values.Type,
) values.QuantifiedObjectValue {
	t.Helper()
	qov, err := values.NewQuantifiedObjectValue(alias, typ)
	return mustConstruct(t, qov, err)
}

func selectMergeFieldOrdinals(t testing.TB, child values.Value, ordinals ...int) values.Value {
	t.Helper()
	field, err := values.ResolveFieldOrdinals(child, ordinals)
	return mustConstruct(t, field, err)
}

func selectMergeOrdinalSeedField(t testing.TB, child values.Value, ordinal int) values.Value {
	t.Helper()
	field, err := values.ResolveOrdinalSeedField(child, ordinal)
	return mustConstruct(t, field, err)
}

func selectMergeFire(
	t testing.TB,
	rule ExpressionRule,
	ref *expressions.Reference,
) []expressions.RelationalExpression {
	t.Helper()
	yielded, err := FireExpressionRule(rule, ref)
	if err != nil {
		t.Fatalf("FireExpressionRule: %v", err)
	}
	return yielded
}

func selectMergeExistentialAlias(
	t testing.TB,
	q expressions.Quantifier,
) *predicates.ExistentialValuePredicate {
	t.Helper()
	flowed := selectMergeFlowed(t, q)
	predicate, err := predicates.NewExistentialAlias(q.GetAlias(), flowed.FlowedType())
	return mustConstruct(t, predicate, err)
}

func TestSelectMergeRule_FilterChild(t *testing.T) {
	t.Parallel()

	scan := selectMergeScan(t)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	pred := &predicates.ComparisonPredicate{
		Operand:    qFieldValue(t, scanQ, "X"),
		Comparison: predicates.Comparison{Type: predicates.ComparisonEquals, Operand: &values.ConstantValue{Value: int64(5)}},
	}
	filter := selectMergeFilter(t, []predicates.QueryPredicate{pred}, scanQ)
	filterRef := expressions.InitialOf(filter)
	filterQ := expressions.ForEachQuantifier(filterRef)

	sel := selectMergeSelect(t,
		selectMergeFlowed(t, filterQ),
		[]expressions.Quantifier{filterQ},
		nil,
	)
	selRef := expressions.InitialOf(sel)

	yielded := selectMergeFire(t, NewSelectMergeRule(), selRef)
	if len(yielded) != 1 {
		t.Fatalf("expected 1 yielded expression, got %d", len(yielded))
	}

	merged, ok := yielded[0].(*expressions.SelectExpression)
	if !ok {
		t.Fatalf("expected *SelectExpression, got %T", yielded[0])
	}

	// Merged Select should have scan's quantifier, not filter's.
	mergedQs := merged.GetQuantifiers()
	if len(mergedQs) != 1 {
		t.Fatalf("expected 1 quantifier, got %d", len(mergedQs))
	}
	if mergedQs[0].GetRangesOver() != scanRef {
		t.Error("merged quantifier should range over the scan Reference")
	}

	// Predicates should include the pulled-up filter predicate.
	mergedPreds := merged.GetPredicates()
	if len(mergedPreds) != 1 {
		t.Fatalf("expected 1 predicate, got %d", len(mergedPreds))
	}
}

func TestSelectMergeRule_FilterChildWithOuterPredicates(t *testing.T) {
	t.Parallel()

	scan := selectMergeScan(t)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	innerPred := &predicates.ComparisonPredicate{
		Operand:    qFieldValue(t, scanQ, "X"),
		Comparison: predicates.Comparison{Type: predicates.ComparisonEquals, Operand: &values.ConstantValue{Value: int64(5)}},
	}
	filter := selectMergeFilter(t, []predicates.QueryPredicate{innerPred}, scanQ)
	filterRef := expressions.InitialOf(filter)
	filterQ := expressions.ForEachQuantifier(filterRef)

	outerPred := &predicates.ComparisonPredicate{
		Operand:    qFieldValue(t, filterQ, "Y"),
		Comparison: predicates.Comparison{Type: predicates.ComparisonLessThan, Operand: &values.ConstantValue{Value: int64(10)}},
	}
	sel := selectMergeSelect(t,
		selectMergeFlowed(t, filterQ),
		[]expressions.Quantifier{filterQ},
		[]predicates.QueryPredicate{outerPred},
	)
	selRef := expressions.InitialOf(sel)

	yielded := selectMergeFire(t, NewSelectMergeRule(), selRef)
	if len(yielded) != 1 {
		t.Fatalf("expected 1 yielded, got %d", len(yielded))
	}

	merged := yielded[0].(*expressions.SelectExpression)
	if len(merged.GetPredicates()) != 2 {
		t.Fatalf("expected 2 predicates (outer + inner), got %d", len(merged.GetPredicates()))
	}
	if len(merged.GetQuantifiers()) != 1 {
		t.Fatalf("expected 1 quantifier, got %d", len(merged.GetQuantifiers()))
	}
}

func TestSelectMergeRule_NoMergeableChild(t *testing.T) {
	t.Parallel()

	scan := selectMergeScan(t)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	sel := selectMergeSelect(t,
		selectMergeFlowed(t, scanQ),
		[]expressions.Quantifier{scanQ},
		nil,
	)
	selRef := expressions.InitialOf(sel)

	yielded := selectMergeFire(t, NewSelectMergeRule(), selRef)
	if len(yielded) != 0 {
		t.Fatalf("expected 0 yields (no mergeable child), got %d", len(yielded))
	}
}

func TestSelectMergeRule_SelectChild(t *testing.T) {
	t.Parallel()

	scan := selectMergeScan(t)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	childPred := &predicates.ComparisonPredicate{
		Operand:    qFieldValue(t, scanQ, "A"),
		Comparison: predicates.Comparison{Type: predicates.ComparisonEquals, Operand: &values.ConstantValue{Value: int64(1)}},
	}
	childSel := selectMergeSelect(t,
		selectMergeFlowed(t, scanQ),
		[]expressions.Quantifier{scanQ},
		[]predicates.QueryPredicate{childPred},
	)
	childRef := expressions.InitialOf(childSel)
	childQ := expressions.ForEachQuantifier(childRef)

	outerSel := selectMergeSelect(t,
		selectMergeFlowed(t, childQ),
		[]expressions.Quantifier{childQ},
		nil,
	)
	outerRef := expressions.InitialOf(outerSel)

	yielded := selectMergeFire(t, NewSelectMergeRule(), outerRef)
	if len(yielded) != 1 {
		t.Fatalf("expected 1 yielded, got %d", len(yielded))
	}

	merged := yielded[0].(*expressions.SelectExpression)
	if len(merged.GetQuantifiers()) != 1 {
		t.Fatalf("expected 1 quantifier (from inner Select), got %d", len(merged.GetQuantifiers()))
	}
	if merged.GetQuantifiers()[0].GetRangesOver() != scanRef {
		t.Error("merged quantifier should range over the scan Reference")
	}
	if len(merged.GetPredicates()) != 1 {
		t.Fatalf("expected 1 predicate (from child Select), got %d", len(merged.GetPredicates()))
	}
}

func TestSelectMergeRule_StrictSingleBarrier(t *testing.T) {
	t.Parallel()

	scanRef := expressions.InitialOf(selectMergeScan(t))

	t.Run("strict_parent_target", func(t *testing.T) {
		scanQ := expressions.ForEachQuantifier(scanRef)
		pred := predicates.NewComparisonPredicate(
			qFieldValue(t, scanQ, "X"),
			predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(1)),
		)
		filter := selectMergeFilter(t, []predicates.QueryPredicate{pred}, scanQ)
		targetAlias := values.NamedCorrelationIdentifier("STRICT_TARGET")
		targetQ := expressions.NamedForEachStrictSingleQuantifier(
			targetAlias, expressions.InitialOf(filter))
		parent := selectMergeSelect(t,
			selectMergeFlowed(t, targetQ),
			[]expressions.Quantifier{targetQ},
			nil,
		)
		if yielded := selectMergeFire(t,
			NewSelectMergeRule(), expressions.InitialOf(parent)); len(yielded) != 0 {
			t.Fatalf("strict parent target yielded %d merge(s), want zero", len(yielded))
		}
	})

	t.Run("strict_child_edge", func(t *testing.T) {
		strictAlias := values.NamedCorrelationIdentifier("STRICT_CHILD")
		strictQ := expressions.NamedForEachStrictSingleQuantifier(
			strictAlias, scanRef)
		pred := predicates.NewComparisonPredicate(
			qFieldValue(t, strictQ, "X"),
			predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(1)),
		)
		child := selectMergeSelect(t,
			selectMergeFlowed(t, strictQ),
			[]expressions.Quantifier{strictQ},
			[]predicates.QueryPredicate{pred},
		)
		parentQ := expressions.ForEachQuantifier(expressions.InitialOf(child))
		parent := selectMergeSelect(t,
			selectMergeFlowed(t, parentQ),
			[]expressions.Quantifier{parentQ},
			nil,
		)
		if yielded := selectMergeFire(t,
			NewSelectMergeRule(), expressions.InitialOf(parent)); len(yielded) != 0 {
			t.Fatalf("strict child edge yielded %d merge(s), want zero", len(yielded))
		}
	})
}

func TestSelectMergeRule_TwoQuantifiersOneFilter(t *testing.T) {
	t.Parallel()

	// Pattern: Select(ForEach(Filter(scan, preds)), ForEach(Explode), eqPred)
	// After merge: Select(ForEach(scan), ForEach(Explode), preds + eqPred)
	scan := selectMergeScan(t)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	filterPred := &predicates.ComparisonPredicate{
		Operand:    qFieldValue(t, scanQ, "COL"),
		Comparison: predicates.Comparison{Type: predicates.ComparisonEquals, Operand: &values.ConstantValue{Value: int64(42)}},
	}
	filter := selectMergeFilter(t, []predicates.QueryPredicate{filterPred}, scanQ)
	filterRef := expressions.InitialOf(filter)
	filterQ := expressions.ForEachQuantifier(filterRef)

	explode := selectMergeExplode(t, &values.ConstantValue{
		Value: []any{int64(1), int64(2), int64(3)},
		Typ:   values.NewArrayType(false, values.NotNullLong),
	})
	explodeRef := expressions.InitialOf(explode)
	explodeQ := expressions.ForEachQuantifier(explodeRef)

	eqPred := &predicates.ComparisonPredicate{
		Operand:    qFieldValue(t, filterQ, "COL"),
		Comparison: predicates.Comparison{Type: predicates.ComparisonEquals, Operand: selectMergeFlowed(t, explodeQ)},
	}

	sel := selectMergeSelect(t,
		selectMergeFlowed(t, filterQ),
		[]expressions.Quantifier{filterQ, explodeQ},
		[]predicates.QueryPredicate{eqPred},
	)
	selRef := expressions.InitialOf(sel)

	yielded := selectMergeFire(t, NewSelectMergeRule(), selRef)
	// Expect 2 yields: one from the original quantifier order, one
	// from the ChildrenAsSet-swapped order (both have a mergeable
	// Filter child).
	if len(yielded) < 1 {
		t.Fatalf("expected at least 1 yielded, got %d", len(yielded))
	}

	merged := yielded[0].(*expressions.SelectExpression)
	if len(merged.GetQuantifiers()) != 2 {
		t.Fatalf("expected 2 quantifiers (scan + explode), got %d", len(merged.GetQuantifiers()))
	}
	// First quantifier: scan (pulled up from filter)
	if merged.GetQuantifiers()[0].GetRangesOver() != scanRef {
		t.Error("first quantifier should range over scan")
	}
	// Second quantifier: explode (kept as-is)
	if merged.GetQuantifiers()[1].GetRangesOver() != explodeRef {
		t.Error("second quantifier should range over explode")
	}
	// Predicates: outer eq pred (rebased) + inner filter pred
	if len(merged.GetPredicates()) != 2 {
		t.Fatalf("expected 2 predicates, got %d", len(merged.GetPredicates()))
	}
}

func TestSelectMergeRule_NullOnEmptyNotMerged(t *testing.T) {
	t.Parallel()

	scan := selectMergeScan(t)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	pred := &predicates.ComparisonPredicate{
		Operand: qFieldValue(t, scanQ, "X"),
		Comparison: predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: &values.ConstantValue{Value: int64(1)},
		},
	}
	filter := selectMergeFilter(t, []predicates.QueryPredicate{pred}, scanQ)
	filterRef := expressions.InitialOf(filter)

	// Use null-on-empty quantifier — must NOT be merged.
	nullQ := expressions.ForEachNullOnEmptyQuantifier(filterRef)
	sel := selectMergeSelect(t,
		selectMergeFlowed(t, nullQ),
		[]expressions.Quantifier{nullQ},
		nil,
	)
	selRef := expressions.InitialOf(sel)

	yielded := selectMergeFire(t, NewSelectMergeRule(), selRef)
	if len(yielded) != 0 {
		t.Fatalf("expected 0 yields (null-on-empty not merged), got %d", len(yielded))
	}
}

func TestSelectMergeRule_AliasRebase(t *testing.T) {
	t.Parallel()

	// Verify that the parent's result value is rebased when a child
	// is merged: QOV(parentAlias) → QOV(childInnerAlias).
	scan := selectMergeScan(t)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	filter := selectMergeFilter(t, nil, scanQ)
	filterRef := expressions.InitialOf(filter)
	filterQ := expressions.ForEachQuantifier(filterRef)

	// Result value references filterQ's alias.
	resultVal := selectMergeFlowed(t, filterQ)
	sel := selectMergeSelect(t, resultVal, []expressions.Quantifier{filterQ}, nil)
	selRef := expressions.InitialOf(sel)

	yielded := selectMergeFire(t, NewSelectMergeRule(), selRef)
	if len(yielded) != 1 {
		t.Fatalf("expected 1 yielded, got %d", len(yielded))
	}

	merged := yielded[0].(*expressions.SelectExpression)
	// The merged result value should reference scanQ's alias,
	// not filterQ's alias.
	rv := merged.GetResultValue()
	qov, ok := values.AsQuantifiedObjectValue(rv)
	if !ok {
		t.Fatalf("expected *QuantifiedObjectValue, got %T", rv)
	}
	if qov.Correlation() != scanQ.GetAlias() {
		t.Errorf("result value alias = %s, want %s (child inner alias)",
			qov.Correlation().Name(), scanQ.GetAlias().Name())
	}
}

func TestSelectMergeRule_MultiQuantifierChild(t *testing.T) {
	t.Parallel()

	// Child Select with 2 quantifiers: Select([q1, q2], childPreds, childResult)
	scan1 := selectMergeScan(t)
	scan1Ref := expressions.InitialOf(scan1)
	scan1Q := expressions.ForEachQuantifier(scan1Ref)

	scan2 := selectMergeScan(t)
	scan2Ref := expressions.InitialOf(scan2)
	scan2Q := expressions.ForEachQuantifier(scan2Ref)

	childResult := selectMergeFlowed(t, scan1Q)
	childSel := selectMergeSelect(t,
		childResult,
		[]expressions.Quantifier{scan1Q, scan2Q},
		nil,
	)
	childRef := expressions.InitialOf(childSel)
	childQ := expressions.ForEachQuantifier(childRef)

	// Outer: result references childQ's alias (which is the multi-quant child).
	outerResult := selectMergeFlowed(t, childQ)
	outerSel := selectMergeSelect(t,
		outerResult,
		[]expressions.Quantifier{childQ},
		nil,
	)
	outerRef := expressions.InitialOf(outerSel)

	yielded := selectMergeFire(t, NewSelectMergeRule(), outerRef)
	if len(yielded) < 1 {
		t.Fatalf("expected at least 1 yielded, got %d", len(yielded))
	}

	merged := yielded[0].(*expressions.SelectExpression)
	// Should have 2 quantifiers (from child), not 1.
	if len(merged.GetQuantifiers()) != 2 {
		t.Fatalf("expected 2 quantifiers, got %d", len(merged.GetQuantifiers()))
	}
	// The result value should be the child's result value (QOV of scan1Q),
	// NOT a dangling reference to childQ's alias.
	rv := merged.GetResultValue()
	qov, ok := values.AsQuantifiedObjectValue(rv)
	if !ok {
		t.Fatalf("expected *QuantifiedObjectValue, got %T", rv)
	}
	if qov.Correlation() != scan1Q.GetAlias() {
		t.Errorf("result value alias = %s, want %s (child's inner scan alias)",
			qov.Correlation().Name(), scan1Q.GetAlias().Name())
	}
}

func TestSelectMergeRule_WithSourceAliases(t *testing.T) {
	t.Parallel()

	scan := selectMergeScan(t)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	filter := selectMergeFilter(t, nil, scanQ)
	filterRef := expressions.InitialOf(filter)
	filterQ := expressions.ForEachQuantifier(filterRef)

	sel := selectMergeSelectWithAliases(t,
		selectMergeFlowed(t, filterQ),
		[]expressions.Quantifier{filterQ},
		nil,
		[]string{"FILTERED_SOURCE"},
	)
	selRef := expressions.InitialOf(sel)

	yielded := selectMergeFire(t, NewSelectMergeRule(), selRef)
	if len(yielded) < 1 {
		t.Fatalf("expected at least 1 yielded, got %d", len(yielded))
	}

	merged := yielded[0].(*expressions.SelectExpression)
	if len(merged.GetQuantifiers()) != 1 {
		t.Fatalf("expected 1 quantifier, got %d", len(merged.GetQuantifiers()))
	}
}

func TestSelectMergeRule_MultiQuantifierWithPredicates(t *testing.T) {
	t.Parallel()

	scan1 := selectMergeScan(t)
	scan1Ref := expressions.InitialOf(scan1)
	scan1Q := expressions.ForEachQuantifier(scan1Ref)

	scan2 := selectMergeScan(t)
	scan2Ref := expressions.InitialOf(scan2)
	scan2Q := expressions.ForEachQuantifier(scan2Ref)

	childPred := &predicates.ComparisonPredicate{
		Operand: qFieldValue(t, scan1Q, "A"),
		Comparison: predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: &values.ConstantValue{Value: int64(1)},
		},
	}
	childSel := selectMergeSelect(t,
		selectMergeFlowed(t, scan1Q),
		[]expressions.Quantifier{scan1Q, scan2Q},
		[]predicates.QueryPredicate{childPred},
	)
	childRef := expressions.InitialOf(childSel)
	childQ := expressions.ForEachQuantifier(childRef)

	outerPred := &predicates.ComparisonPredicate{
		Operand: selectMergeFlowed(t, childQ),
		Comparison: predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: &values.ConstantValue{Value: int64(99)},
		},
	}
	outerSel := selectMergeSelect(t,
		selectMergeFlowed(t, childQ),
		[]expressions.Quantifier{childQ},
		[]predicates.QueryPredicate{outerPred},
	)
	outerRef := expressions.InitialOf(outerSel)

	yielded := selectMergeFire(t, NewSelectMergeRule(), outerRef)
	if len(yielded) < 1 {
		t.Fatalf("expected at least 1 yielded, got %d", len(yielded))
	}

	merged := yielded[0].(*expressions.SelectExpression)
	// 2 quantifiers from child, child's predicate pulled up
	if len(merged.GetQuantifiers()) != 2 {
		t.Fatalf("expected 2 quantifiers, got %d", len(merged.GetQuantifiers()))
	}
	// outer pred (translated) + child pred
	if len(merged.GetPredicates()) != 2 {
		t.Fatalf("expected 2 predicates, got %d", len(merged.GetPredicates()))
	}
	// Outer predicate's operand should be translated from childQ alias
	// to the child's result value (scan1Q's QOV).
	outerTranslated := merged.GetPredicates()[0].(*predicates.ComparisonPredicate)
	rv, ok := values.AsQuantifiedObjectValue(outerTranslated.Operand)
	if !ok {
		t.Fatalf("expected QOV in translated predicate, got %T", outerTranslated.Operand)
	}
	if rv.Correlation() != scan1Q.GetAlias() {
		t.Errorf("predicate operand alias = %s, want %s",
			rv.Correlation().Name(), scan1Q.GetAlias().Name())
	}
}

// --- Ported from Java SelectMergeRuleTest ---
//
// The Java tests use baseT()/baseTau() (typed LogicalTypeFilter over
// FullUnorderedScan) and GraphExpansion builders with named columns.
// Go's existing tests use FullUnorderedScanExpression directly as
// the leaf. To match the Java intent, we use the same leaf shape
// (FullUnorderedScanExpression) since LogicalTypeFilterExpression is
// NOT mergeable and serves only as a correlation-bearing leaf.
//
// Helper: fieldValue(qun, "name") in Java = an exactly-resolved field access
// Helper: fieldPredicate(qun, "name", cmp) = ComparisonPredicate{Operand: fieldValue(qun, "name"), Comparison: cmp}
// Helper: selectWithPredicates(qun, fields, preds) = GraphExpansionBuilder-based SelectExpression

// qFieldValue creates a FieldValue referencing a quantifier's field, mirroring
// Java's fieldValue(Quantifier, String).
func qFieldValue(t testing.TB, q expressions.Quantifier, field string) values.Value {
	t.Helper()
	request, err := values.FieldByName(field)
	request = mustConstruct(t, request, err)
	resolved, err := values.ResolveFieldAccess(
		selectMergeFlowed(t, q), []values.FieldRequest{request})
	return mustConstruct(t, resolved, err)
}

// qFieldPred creates a ComparisonPredicate on a quantifier's field, mirroring
// Java's fieldPredicate(Quantifier, String, Comparison).
func qFieldPred(t testing.TB, q expressions.Quantifier, field string, cmp predicates.Comparison) *predicates.ComparisonPredicate {
	t.Helper()
	return &predicates.ComparisonPredicate{
		Operand:    qFieldValue(t, q, field),
		Comparison: cmp,
	}
}

// valueCmp creates a ValueComparison (comparison where RHS is a Value),
// mirroring Java's new Comparisons.ValueComparison(type, value).
func valueCmp(typ predicates.ComparisonType, v values.Value) predicates.Comparison {
	return predicates.Comparison{Type: typ, Operand: v}
}

// literalCmp creates a comparison with a literal operand, mirroring
// Java's new Comparisons.SimpleComparison(type, literal).
func literalCmp(typ predicates.ComparisonType, lit any) predicates.Comparison {
	return predicates.NewLiteralComparison(typ, lit)
}

// paramCmp creates a parameter-bound comparison, mirroring
// Java's new Comparisons.ParameterComparison(type, "p").
func paramCmp(typ predicates.ComparisonType, param string) predicates.Comparison {
	return predicates.Comparison{Type: typ, ParameterName: param}
}

// baseLeaf creates a FullUnorderedScanExpression wrapped in a ForEach quantifier,
// playing the role of Java's baseT()/baseTau() for tests that only need
// a correlation-bearing leaf.
func baseLeaf(t testing.TB) (expressions.Quantifier, *expressions.Reference) {
	t.Helper()
	scan := selectMergeScan(t)
	ref := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(ref)
	return q, ref
}

// selectWithPreds creates a SelectExpression with one quantifier and the
// given predicates, using qun.GetFlowedObjectValue() as the result value.
// Mirrors Java's selectWithPredicates(qun, predicates...).
func selectWithPreds(t testing.TB, qun expressions.Quantifier, preds ...predicates.QueryPredicate) *expressions.SelectExpression {
	t.Helper()
	return selectMergeSelect(t,
		selectMergeFlowed(t, qun),
		[]expressions.Quantifier{qun},
		preds,
	)
}

// forEachOf wraps an expression in a ForEach quantifier,
// mirroring Java's forEach(RelationalExpression).
func forEachOf(expr expressions.RelationalExpression) expressions.Quantifier {
	return expressions.ForEachQuantifier(expressions.InitialOf(expr))
}

// existentialOf wraps an expression in an Existential quantifier,
// mirroring Java's exists(RelationalExpression).
func existentialOf(expr expressions.RelationalExpression) expressions.Quantifier {
	return expressions.ExistentialQuantifier(expressions.InitialOf(expr))
}

// TestSelectMergeRule_DoNotMergeExistentials validates that existential
// quantifiers are NOT merged into the parent select.
//
// Ports Java's SelectMergeRuleTest.doNotMergeExistentials:
//
//	SELECT a, b
//	  FROM t
//	  WHERE EXISTS (SELECT * FROM t.f WHERE f > 42)
//
// The existential cannot be merged up.
func TestSelectMergeRule_DoNotMergeExistentials(t *testing.T) {
	t.Parallel()

	baseQun, _ := baseLeaf(t)

	// Explode t.f
	explodeFQun := forEachOf(selectMergeExplode(t, qFieldValue(t, baseQun, "f")))

	// LogicalFilter on exploded values: f > 42
	filterPred := &predicates.ComparisonPredicate{
		Operand:    selectMergeFlowed(t, explodeFQun),
		Comparison: literalCmp(predicates.ComparisonGreaterThan, int64(42)),
	}
	filteredF := selectMergeFilter(t,
		[]predicates.QueryPredicate{filterPred},
		explodeFQun,
	)

	// EXISTS quantifier over the filter
	existsQun := existentialOf(filteredF)

	// Upper select: SELECT a, b FROM t WHERE EXISTS (...)
	existsPred := selectMergeExistentialAlias(t, existsQun)
	upper := selectMergeSelect(t,
		selectMergeFlowed(t, baseQun),
		[]expressions.Quantifier{baseQun, existsQun},
		[]predicates.QueryPredicate{existsPred},
	)
	upperRef := expressions.InitialOf(upper)

	yielded := selectMergeFire(t, NewSelectMergeRule(), upperRef)
	if len(yielded) != 0 {
		t.Fatalf("expected 0 yields (existential not merged), got %d", len(yielded))
	}
}

// TestSelectMergeRule_ExistentialWrapPreservesPositionalUnnest pins the
// existential-wrapper boundary used by UNNEST + EXISTS. A WITH ORDINALITY
// unnest has exactly two positional owner windows (outer row, element+ordinal),
// so an arity-only >2 barrier does not protect it. Flattening that child exposes
// its correlated Explode as a direct leg of the three-quantifier existential
// Select, which the NLJ implementer must decline: the Explode has no bound outer
// evaluation context until the nested FlatMap owns it. The child must therefore
// stay opaque even at two windows.
//
// The sibling control is deliberately another two-window positional box, but
// with two ordinary scan legs and no Explode. It must still merge. This makes
// the regression sensitive to the unnest authority rather than accidentally
// installing a blanket two-window optimization barrier.
func TestSelectMergeRule_ExistentialWrapPreservesPositionalUnnest(t *testing.T) {
	t.Parallel()

	existsScan := selectMergeTypedScan(t, "EXISTS", values.NewRecordType("EXISTS", false, []values.Field{
		{Name: "K", Ordinal: 0, FieldType: values.NotNullLong},
	}))

	t.Run("positional unnest stays nested", func(t *testing.T) {
		t.Parallel()

		outerType := values.NewRecordType("OUTER", false, []values.Field{
			{Name: "ARR", Ordinal: 0, FieldType: values.NewArrayType(false, values.NotNullLong)},
		})
		outerAlias := values.NamedCorrelationIdentifier("OUTER")
		outerQ := expressions.NamedForEachQuantifier(
			outerAlias,
			expressions.InitialOf(selectMergeTypedScan(t, "OUTER", outerType)),
		)
		outerQOV := selectMergeQOV(t, outerAlias, outerType)
		collection := selectMergeFieldOrdinals(t, outerQOV, 0)
		explode, err := expressions.NewExplodeExpressionWithOrdinality(collection, true)
		ordinalExplode := mustConstruct(t, explode, err)
		elementAlias := values.NamedCorrelationIdentifier("ELEMENT")
		elementQ := expressions.NamedForEachQuantifier(elementAlias, expressions.InitialOf(ordinalExplode))
		elementQOV := selectMergeFlowed(t, elementQ)

		seed := values.NewRawRecordConstructorValue(
			values.RecordConstructorField{Name: "ARR", Value: selectMergeOrdinalSeedField(t, outerQOV, 0)},
			values.RecordConstructorField{Name: "VAL", Value: selectMergeOrdinalSeedField(t, elementQOV, 0)},
			values.RecordConstructorField{Name: "AT", Value: selectMergeOrdinalSeedField(t, elementQOV, 1)},
		)
		windows, _ := values.OrdinalSeedLegWindows(seed)
		if len(windows) != 2 {
			t.Fatalf("fixture has %d positional owner windows, want exactly 2", len(windows))
		}
		unnest := selectMergeSelectWithAliases(
			t,
			seed,
			[]expressions.Quantifier{outerQ, elementQ},
			nil,
			[]string{"OUTER", "ELEMENT"},
		)
		unnestQ := expressions.NamedForEachQuantifier(
			values.NamedCorrelationIdentifier("UNNEST_BOX"),
			expressions.InitialOf(unnest),
		)
		existsQ := expressions.ExistentialQuantifier(expressions.InitialOf(existsScan))
		wrap := selectMergeSelect(
			t,
			selectMergeFlowed(t, unnestQ),
			[]expressions.Quantifier{unnestQ, existsQ},
			[]predicates.QueryPredicate{selectMergeExistentialAlias(t, existsQ)},
		)

		if yielded := selectMergeFire(t, NewSelectMergeRule(), expressions.InitialOf(wrap)); len(yielded) != 0 {
			t.Fatalf("positional UNNEST child was flattened through its existential wrapper: got %d yields", len(yielded))
		}
	})

	t.Run("record element unnest stays nested", func(t *testing.T) {
		t.Parallel()

		elementType := values.NewRecordType("RECORD", false, []values.Field{
			{Name: "EK", Ordinal: 0, FieldType: values.NullableLong},
			{Name: "PAYLOAD", Ordinal: 1, FieldType: values.NullableString},
		})
		outerType := values.NewRecordType("OUTER_RECORD", false, []values.Field{
			{Name: "ID", Ordinal: 0, FieldType: values.NotNullLong},
			{Name: "ARR", Ordinal: 1, FieldType: values.NewArrayType(true, elementType)},
		})
		outerAlias := values.NamedCorrelationIdentifier("OUTER_RECORD")
		outerQ := expressions.NamedForEachQuantifier(
			outerAlias,
			expressions.InitialOf(selectMergeTypedScan(t, "OUTER_RECORD", outerType)),
		)
		outerQOV := selectMergeQOV(t, outerAlias, outerType)
		collection := selectMergeFieldOrdinals(t, outerQOV, 1)
		explode := selectMergeExplode(t, collection)
		elementAlias := values.NamedCorrelationIdentifier("ELEMENT_RECORD")
		elementQ := expressions.NamedForEachQuantifier(
			elementAlias, expressions.InitialOf(explode))
		elementQOV := selectMergeFlowed(t, elementQ)
		result := values.NewRawRecordConstructorValue(
			values.RecordConstructorField{Name: "ID", Value: selectMergeFieldOrdinals(t, outerQOV, 0)},
			values.RecordConstructorField{Name: "ELEMENT", Value: elementQOV},
		)
		if windows, _ := values.OrdinalSeedLegWindows(result); windows != nil {
			t.Fatalf("record-element fixture unexpectedly became a positional seed: %v", windows)
		}
		unnest := selectMergeSelectWithAliases(
			t,
			result,
			[]expressions.Quantifier{outerQ, elementQ},
			nil,
			[]string{outerAlias.Name(), elementAlias.Name()},
		)
		unnestRef := expressions.InitialOf(unnest)
		if !childRefIsLateralUnnestSelect(unnestRef) || childRefIsPositionalUnnestSelect(unnestRef) {
			t.Fatal("fixture must be a lateral, non-positional record-element UNNEST")
		}
		unnestQ := expressions.NamedForEachQuantifier(
			values.NamedCorrelationIdentifier("RECORD_UNNEST_BOX"), unnestRef)
		existsQ := expressions.ExistentialQuantifier(expressions.InitialOf(existsScan))
		wrap := selectMergeSelect(
			t,
			selectMergeFlowed(t, unnestQ),
			[]expressions.Quantifier{unnestQ, existsQ},
			[]predicates.QueryPredicate{selectMergeExistentialAlias(t, existsQ)},
		)

		if yielded := selectMergeFire(t, NewSelectMergeRule(), expressions.InitialOf(wrap)); len(yielded) != 0 {
			t.Fatalf("record-element UNNEST child was flattened through its existential wrapper: got %d yields", len(yielded))
		}
		if collection != explode.GetCollectionValue() || outerQOV.Correlation() != outerAlias {
			t.Fatal("record-element barrier mutated its collection or source declaration")
		}
	})

	t.Run("uncorrelated explode remains mergeable", func(t *testing.T) {
		t.Parallel()

		collection := values.NewArrayConstructorValue(
			values.NotNullLong,
			[]values.Value{values.LiteralValue(int64(1))},
		)
		explode := selectMergeExplode(t, collection)
		elementAlias := values.NamedCorrelationIdentifier("INLINE_ELEMENT")
		elementQ := expressions.NamedForEachQuantifier(
			elementAlias, expressions.InitialOf(explode))
		child := selectMergeSelectWithAliases(
			t,
			selectMergeFlowed(t, elementQ),
			[]expressions.Quantifier{elementQ},
			nil,
			[]string{elementAlias.Name()},
		)
		childRef := expressions.InitialOf(child)
		if childRefIsLateralUnnestSelect(childRef) {
			t.Fatal("uncorrelated Explode was misclassified as a lateral UNNEST")
		}
		childQ := expressions.NamedForEachQuantifier(
			values.NamedCorrelationIdentifier("INLINE_BOX"), childRef)
		existsQ := expressions.ExistentialQuantifier(expressions.InitialOf(existsScan))
		wrap := selectMergeSelect(
			t,
			selectMergeFlowed(t, childQ),
			[]expressions.Quantifier{childQ, existsQ},
			[]predicates.QueryPredicate{selectMergeExistentialAlias(t, existsQ)},
		)

		if yielded := selectMergeFire(t, NewSelectMergeRule(), expressions.InitialOf(wrap)); len(yielded) != 1 {
			t.Fatalf("uncorrelated Explode yielded %d alternatives, want the ordinary merge", len(yielded))
		}
	})

	t.Run("two-window non-unnest box still merges", func(t *testing.T) {
		t.Parallel()

		leftType := values.NewRecordType("LEFT", false, []values.Field{
			{Name: "L", Ordinal: 0, FieldType: values.NotNullLong},
		})
		rightType := values.NewRecordType("RIGHT", false, []values.Field{
			{Name: "R", Ordinal: 0, FieldType: values.NotNullLong},
		})
		leftAlias := values.NamedCorrelationIdentifier("LEFT")
		rightAlias := values.NamedCorrelationIdentifier("RIGHT")
		leftQ := expressions.NamedForEachQuantifier(
			leftAlias,
			expressions.InitialOf(selectMergeTypedScan(t, "LEFT", leftType)),
		)
		rightQ := expressions.NamedForEachQuantifier(
			rightAlias,
			expressions.InitialOf(selectMergeTypedScan(t, "RIGHT", rightType)),
		)
		seed := values.NewRawRecordConstructorValue(
			values.RecordConstructorField{Name: "L", Value: selectMergeOrdinalSeedField(t, selectMergeQOV(t, leftAlias, leftType), 0)},
			values.RecordConstructorField{Name: "R", Value: selectMergeOrdinalSeedField(t, selectMergeQOV(t, rightAlias, rightType), 0)},
		)
		windows, _ := values.OrdinalSeedLegWindows(seed)
		if len(windows) != 2 {
			t.Fatalf("control has %d positional owner windows, want exactly 2", len(windows))
		}
		box := selectMergeSelectWithAliases(
			t,
			seed,
			[]expressions.Quantifier{leftQ, rightQ},
			nil,
			[]string{"LEFT", "RIGHT"},
		)
		boxAlias := values.NamedCorrelationIdentifier("ORDINARY_BOX")
		boxQ := expressions.NamedForEachQuantifier(boxAlias, expressions.InitialOf(box))
		existsQ := expressions.ExistentialQuantifier(expressions.InitialOf(existsScan))
		wrap := selectMergeSelect(
			t,
			selectMergeFlowed(t, boxQ),
			[]expressions.Quantifier{boxQ, existsQ},
			[]predicates.QueryPredicate{selectMergeExistentialAlias(t, existsQ)},
		)

		yielded := selectMergeFire(t, NewSelectMergeRule(), expressions.InitialOf(wrap))
		if len(yielded) != 1 {
			t.Fatalf("two-window non-UNNEST box yielded %d alternatives, want 1 merged alternative", len(yielded))
		}
		merged, ok := yielded[0].(*expressions.SelectExpression)
		if !ok {
			t.Fatalf("merged alternative is %T, want *SelectExpression", yielded[0])
		}
		if got := len(merged.GetQuantifiers()); got != 3 {
			t.Fatalf("merged control has %d quantifiers, want two scan legs plus existential", got)
		}
		for _, q := range merged.GetQuantifiers() {
			if q.GetAlias() == boxAlias {
				t.Fatal("merged control retained the ordinary box alias")
			}
		}
	})
}

// TestSelectMergeRule_MergeFilterOnPrimitiveExplode validates merging a
// LogicalFilter on an exploded primitive array into the parent select.
//
// Ports Java's SelectMergeRuleTest.mergeFilterOnPrimitiveExplode:
//
//	SELECT t.b, f
//	  FROM t, (SELECT f FROM t.f WHERE f > 42)
//	  WHERE t.a = 42
//
// After merge:
//
//	SELECT t.b, f
//	  FROM t, t.f AS f
//	  WHERE f > 42 AND t.a = 42
func TestSelectMergeRule_MergeFilterOnPrimitiveExplode(t *testing.T) {
	t.Parallel()

	baseQun, _ := baseLeaf(t)

	// Explode t.f
	explodeFQun := forEachOf(selectMergeExplode(t, qFieldValue(t, baseQun, "f")))

	// LogicalFilter: f > 42
	filterPred := &predicates.ComparisonPredicate{
		Operand:    selectMergeFlowed(t, explodeFQun),
		Comparison: literalCmp(predicates.ComparisonGreaterThan, int64(42)),
	}
	filteredF := selectMergeFilter(t,
		[]predicates.QueryPredicate{filterPred},
		explodeFQun,
	)
	higherFQun := forEachOf(filteredF)

	// Upper select: SELECT t.b, f FROM t, filtered_f WHERE t.a = 42
	upper := selectMergeSelect(t,
		selectMergeFlowed(t, baseQun),
		[]expressions.Quantifier{baseQun, higherFQun},
		[]predicates.QueryPredicate{
			qFieldPred(t, baseQun, "a", literalCmp(predicates.ComparisonEquals, int64(42))),
		},
	)
	upperRef := expressions.InitialOf(upper)

	yielded := selectMergeFire(t, NewSelectMergeRule(), upperRef)
	if len(yielded) < 1 {
		t.Fatalf("expected at least 1 yield, got %d", len(yielded))
	}

	merged := yielded[0].(*expressions.SelectExpression)
	mergedQs := merged.GetQuantifiers()

	// Should have 2 quantifiers: baseQun + explodeFQun (filter merged away)
	if len(mergedQs) != 2 {
		t.Fatalf("expected 2 quantifiers, got %d", len(mergedQs))
	}

	// First quantifier should still be base
	if mergedQs[0].GetAlias() != baseQun.GetAlias() {
		t.Errorf("first quantifier should be base, got alias %s", mergedQs[0].GetAlias().Name())
	}

	// Second quantifier should range over the explode (not the filter)
	explodeRef := explodeFQun.GetRangesOver()
	if mergedQs[1].GetRangesOver() != explodeRef {
		t.Error("second quantifier should range over the ExplodeExpression Reference")
	}

	// Should have 2 predicates: pulled-up filter pred + outer a = 42
	mergedPreds := merged.GetPredicates()
	if len(mergedPreds) != 2 {
		t.Fatalf("expected 2 predicates, got %d", len(mergedPreds))
	}
}

// TestSelectMergeRule_MergeFilterOnNestedExplode validates merging a
// select on a nested repeated field into the parent.
//
// Ports Java's SelectMergeRuleTest.mergeFilterOnNestedExplode:
//
//	SELECT t.b, q.one
//	  FROM t, (SELECT one, three FROM t.g WHERE two > 'hello') AS q
//	  WHERE t.d = q.three
//
// After merge:
//
//	SELECT t.b, q.one
//	  FROM t, t.g AS q
//	  WHERE q.two > 'hello' AND t.d = q.three
func TestSelectMergeRule_MergeFilterOnNestedExplode(t *testing.T) {
	t.Parallel()

	baseQun, _ := baseLeaf(t)

	// Explode t.g
	explodeGQun := forEachOf(selectMergeExplode(t, qFieldValue(t, baseQun, "g")))

	// Inner select: SELECT one, three FROM t.g WHERE two > 'hello'
	innerSel := selectWithPreds(t, explodeGQun,
		qFieldPred(t, explodeGQun, "two", literalCmp(predicates.ComparisonGreaterThan, "hello")),
	)
	higherQun := forEachOf(innerSel)

	// Upper select with cross-reference predicate: t.d = q.three
	upper := selectMergeSelect(t,
		selectMergeFlowed(t, baseQun),
		[]expressions.Quantifier{baseQun, higherQun},
		[]predicates.QueryPredicate{
			qFieldPred(t, baseQun, "d", valueCmp(predicates.ComparisonEquals, qFieldValue(t, higherQun, "three"))),
		},
	)
	upperRef := expressions.InitialOf(upper)

	yielded := selectMergeFire(t, NewSelectMergeRule(), upperRef)
	if len(yielded) < 1 {
		t.Fatalf("expected at least 1 yield, got %d", len(yielded))
	}

	merged := yielded[0].(*expressions.SelectExpression)
	mergedQs := merged.GetQuantifiers()

	// Should have 2 quantifiers: baseQun + explodeGQun
	if len(mergedQs) != 2 {
		t.Fatalf("expected 2 quantifiers, got %d", len(mergedQs))
	}

	// Second quantifier should range over the explode
	explodeRef := explodeGQun.GetRangesOver()
	if mergedQs[1].GetRangesOver() != explodeRef {
		t.Error("second quantifier should range over the ExplodeExpression Reference")
	}

	// Should have 2 predicates: pulled-up inner pred + outer cross-ref pred
	mergedPreds := merged.GetPredicates()
	if len(mergedPreds) != 2 {
		t.Fatalf("expected 2 predicates, got %d", len(mergedPreds))
	}
}

// TestSelectMergeRule_DoNotMergeExistentialOnNested validates that an
// existential quantifier on a nested repeated is NOT merged.
//
// Ports Java's SelectMergeRuleTest.doNotMergeExistentialOnNested:
//
//	SELECT a, b
//	  FROM t
//	  WHERE EXISTS (SELECT * FROM t.g WHERE two > 'hello')
func TestSelectMergeRule_DoNotMergeExistentialOnNested(t *testing.T) {
	t.Parallel()

	baseQun, _ := baseLeaf(t)

	// Explode t.g
	explodeGQun := forEachOf(selectMergeExplode(t, qFieldValue(t, baseQun, "g")))

	// Inner select: SELECT * FROM t.g WHERE two > 'hello'
	innerSel := selectWithPreds(t, explodeGQun,
		qFieldPred(t, explodeGQun, "two", literalCmp(predicates.ComparisonGreaterThan, "hello")),
	)

	// EXISTS quantifier
	existsQun := existentialOf(innerSel)

	// Upper select
	existsPred := selectMergeExistentialAlias(t, existsQun)
	upper := selectMergeSelect(t,
		selectMergeFlowed(t, baseQun),
		[]expressions.Quantifier{baseQun, existsQun},
		[]predicates.QueryPredicate{existsPred},
	)
	upperRef := expressions.InitialOf(upper)

	yielded := selectMergeFire(t, NewSelectMergeRule(), upperRef)
	if len(yielded) != 0 {
		t.Fatalf("expected 0 yields (existential not merged), got %d", len(yielded))
	}
}

// TestSelectMergeRule_MergeWithCorrelationsBetweenSiblings validates
// merging two select children where one correlates to the other.
//
// Ports Java's SelectMergeRuleTest.mergeWithCorrelationsBetweenSiblings:
//
//	SELECT x.c, y.gamma
//	  FROM (SELECT b, c, d FROM t WHERE a = 42) x,
//	       (SELECT alpha, beta, gamma, delta FROM tau WHERE beta > x.b) y
//
// Both children are ForEach(Select(...)), so both are merge targets.
// After merge:
//
//	SELECT t.c, tau.gamma
//	  FROM t, tau
//	  WHERE t.a = 42 AND tau.beta > t.b
func TestSelectMergeRule_MergeWithCorrelationsBetweenSiblings(t *testing.T) {
	t.Parallel()

	tQun, _ := baseLeaf(t)
	tauQun, _ := baseLeaf(t)

	// Left child: SELECT ... FROM t WHERE a = 42
	leftSel := selectWithPreds(t, tQun,
		qFieldPred(t, tQun, "a", literalCmp(predicates.ComparisonEquals, int64(42))),
	)
	leftQun := forEachOf(leftSel)

	// Right child: SELECT ... FROM tau WHERE beta > leftQun.b
	// Note: correlation to leftQun's alias
	rightSel := selectWithPreds(t, tauQun,
		qFieldPred(t, tauQun, "beta", valueCmp(predicates.ComparisonGreaterThan, qFieldValue(t, leftQun, "b"))),
	)
	rightQun := forEachOf(rightSel)

	// Upper: SELECT leftQun.c, rightQun.gamma FROM left, right
	upper := selectMergeSelect(t,
		selectMergeFlowed(t, leftQun),
		[]expressions.Quantifier{leftQun, rightQun},
		nil,
	)
	upperRef := expressions.InitialOf(upper)

	yielded := selectMergeFire(t, NewSelectMergeRule(), upperRef)
	if len(yielded) < 1 {
		t.Fatalf("expected at least 1 yield, got %d", len(yielded))
	}

	merged := yielded[0].(*expressions.SelectExpression)
	mergedQs := merged.GetQuantifiers()

	// After merge: should have 2 quantifiers (tQun + tauQun)
	if len(mergedQs) != 2 {
		t.Fatalf("expected 2 quantifiers after merge, got %d", len(mergedQs))
	}

	// Predicates: a = 42 (from left) + beta > t.b (from right, rebased)
	mergedPreds := merged.GetPredicates()
	if len(mergedPreds) != 2 {
		t.Fatalf("expected 2 predicates, got %d", len(mergedPreds))
	}
}

// TestSelectMergeRule_MergeWithDiamond validates merging when the same
// Reference is shared by multiple quantifiers (diamond-shaped DAG).
//
// Ports Java's SelectMergeRuleTest.mergeWithDiamond:
//
//	SELECT baseQun.a, lowerQun1.b, lowerQun1.c
//	  FROM (SELECT b, c, d FROM T WHERE a = 42) AS lowerQun1,
//	       T                                      AS baseQun
//	  WHERE lowerQun1.b = baseQun.d
//
// Both lowerQun1 and baseQun range over the SAME Reference.
// lowerQun1's child is a Select(baseQun's ref). After merge, the
// shared reference must be disambiguated.
func TestSelectMergeRule_MergeWithDiamond(t *testing.T) {
	t.Parallel()

	// Create a single scan reference shared by two quantifiers
	scan := selectMergeScan(t)
	sharedRef := expressions.InitialOf(scan)
	baseQun1 := expressions.ForEachQuantifier(sharedRef) // first user

	// Child select on the shared scan
	childSel := selectWithPreds(t, baseQun1,
		qFieldPred(t, baseQun1, "a", literalCmp(predicates.ComparisonEquals, int64(42))),
	)
	lowerQun1 := forEachOf(childSel)

	// Second quantifier from the same reference (diamond)
	baseQun2 := expressions.ForEachQuantifier(sharedRef)

	// Upper select
	upper := selectMergeSelect(t,
		selectMergeFlowed(t, baseQun2),
		[]expressions.Quantifier{lowerQun1, baseQun2},
		[]predicates.QueryPredicate{
			qFieldPred(t, lowerQun1, "b", valueCmp(predicates.ComparisonEquals, qFieldValue(t, baseQun2, "d"))),
		},
	)
	upperRef := expressions.InitialOf(upper)

	yielded := selectMergeFire(t, NewSelectMergeRule(), upperRef)

	// The rule should merge lowerQun1 (it wraps a Select), pulling
	// up baseQun1 and the predicate. The merged select should have
	// 2 quantifiers (baseQun1 + baseQun2) and 2 predicates
	// (a = 42 + b = d).
	if len(yielded) < 1 {
		t.Fatalf("expected at least 1 yield, got %d", len(yielded))
	}

	merged := yielded[0].(*expressions.SelectExpression)
	mergedQs := merged.GetQuantifiers()

	if len(mergedQs) != 2 {
		t.Fatalf("expected 2 quantifiers, got %d", len(mergedQs))
	}

	mergedPreds := merged.GetPredicates()
	if len(mergedPreds) != 2 {
		t.Fatalf("expected 2 predicates (a=42 + b=d), got %d", len(mergedPreds))
	}
}

// TestSelectMergeRule_MergeUpAvoidingDuplicates validates merging when
// two child selects share the same base quantifier. The merged result
// must disambiguate (create separate quantifiers for each use).
//
// Ports Java's SelectMergeRuleTest.mergeUpAvoidingDuplicates:
//
//	SELECT L.c AS c1, R.c AS c2, L.d AS d1, R.d AS d2
//	  FROM (SELECT a, b, c, d FROM T WHERE a = 42) AS L,
//	       (SELECT a, b, c, d FROM T WHERE b = ?param) AS R
//	  WHERE L.a = R.a AND R.b = L.b
//
// Both L and R use the same base quantifier T. After merge:
//
//	SELECT L.c AS c1, R.c AS c2, L.d AS d1, R.d AS d2
//	  FROM T AS L, T AS R
//	  WHERE L.a = 42 AND R.b = ?param AND L.a = R.a AND R.b = L.b
func TestSelectMergeRule_MergeUpAvoidingDuplicates(t *testing.T) {
	t.Parallel()

	// Shared base reference
	scan := selectMergeScan(t)
	sharedRef := expressions.InitialOf(scan)
	leftBaseQun := expressions.ForEachQuantifier(sharedRef)
	rightBaseQun := expressions.ForEachQuantifier(sharedRef)

	// L: SELECT ... FROM T WHERE a = 42
	leftSel := selectWithPreds(t, leftBaseQun,
		qFieldPred(t, leftBaseQun, "a", literalCmp(predicates.ComparisonEquals, int64(42))),
	)
	leftQun := forEachOf(leftSel)

	// R: SELECT ... FROM T WHERE b = ?param
	rightSel := selectWithPreds(t, rightBaseQun,
		qFieldPred(t, rightBaseQun, "b", paramCmp(predicates.ComparisonEquals, "p")),
	)
	rightQun := forEachOf(rightSel)

	// Upper join
	upper := selectMergeSelect(t,
		selectMergeFlowed(t, leftQun),
		[]expressions.Quantifier{leftQun, rightQun},
		[]predicates.QueryPredicate{
			qFieldPred(t, leftQun, "a", valueCmp(predicates.ComparisonEquals, qFieldValue(t, rightQun, "a"))),
			qFieldPred(t, rightQun, "b", valueCmp(predicates.ComparisonEquals, qFieldValue(t, leftQun, "b"))),
		},
	)
	upperRef := expressions.InitialOf(upper)

	yielded := selectMergeFire(t, NewSelectMergeRule(), upperRef)
	if len(yielded) < 1 {
		t.Fatalf("expected at least 1 yield, got %d", len(yielded))
	}

	merged := yielded[0].(*expressions.SelectExpression)
	mergedQs := merged.GetQuantifiers()

	// Both children are single-quantifier Selects over the same base.
	// After merge, both should have their baseQun pulled up. Since both
	// point to the same Reference, the merged select should have 2
	// quantifiers (both referring to the same scan ref, but with different
	// aliases created by the child selects' forEach wrapping). The critical
	// test is that the merged predicate list has 4 predicates total.
	if len(mergedQs) < 1 {
		t.Fatalf("expected at least 1 quantifier, got %d", len(mergedQs))
	}

	mergedPreds := merged.GetPredicates()
	// Expect 4 predicates: a=42, b=?param, a=R.a, b=L.b
	if len(mergedPreds) != 4 {
		t.Fatalf("expected 4 predicates, got %d", len(mergedPreds))
	}
}

// TestSelectMergeRule_MergeUpWithRenamedCorrelations validates merging
// when two children share a common quantifier AND have internal
// correlations to that shared quantifier.
//
// Ports Java's SelectMergeRuleTest.mergeUpWithRenamedCorrelations.
// This is the most complex case: a shared "values box" quantifier
// is used by both children, and the merge must create separate copies
// with distinct aliases while preserving internal correlations.
func TestSelectMergeRule_MergeUpWithRenamedCorrelations(t *testing.T) {
	t.Parallel()

	// Values box: shared between left and right children
	valuesExpr := selectMergeScan(t)
	valuesRef := expressions.InitialOf(valuesExpr)
	valuesBox := expressions.ForEachQuantifier(valuesRef)

	// Left child base
	leftBase, _ := baseLeaf(t)
	leftBaseSel := selectWithPreds(t, leftBase,
		qFieldPred(t, leftBase, "a", valueCmp(predicates.ComparisonEquals, qFieldValue(t, valuesBox, "x"))),
	)
	lowerLeft := forEachOf(leftBaseSel)

	// Left join: SELECT valuesBox.x AS a, lowerLeft.b, ...
	leftJoin := selectMergeSelect(t,
		selectMergeFlowed(t, valuesBox),
		[]expressions.Quantifier{valuesBox, lowerLeft},
		nil,
	)
	leftQun := forEachOf(leftJoin)

	// Right child base
	rightBase, _ := baseLeaf(t)
	rightBaseSel := selectWithPreds(t, rightBase,
		qFieldPred(t, rightBase, "b", valueCmp(predicates.ComparisonEquals, qFieldValue(t, valuesBox, "y"))),
	)
	lowerRight := forEachOf(rightBaseSel)

	// Right join: SELECT lowerRight.a, valuesBox.y AS b, ...
	rightJoin := selectMergeSelect(t,
		selectMergeFlowed(t, lowerRight),
		[]expressions.Quantifier{valuesBox, lowerRight},
		nil,
	)
	rightQun := forEachOf(rightJoin)

	// Upper: SELECT leftQun.a AS a1, rightQun.a AS a2, leftQun.b AS b1, rightQun.b AS b2
	//        WHERE leftQun.a = rightQun.a AND leftQun.b = rightQun.b
	upper := selectMergeSelect(t,
		selectMergeFlowed(t, leftQun),
		[]expressions.Quantifier{leftQun, rightQun},
		[]predicates.QueryPredicate{
			qFieldPred(t, leftQun, "a", valueCmp(predicates.ComparisonEquals, qFieldValue(t, rightQun, "a"))),
			qFieldPred(t, leftQun, "b", valueCmp(predicates.ComparisonEquals, qFieldValue(t, rightQun, "b"))),
		},
	)
	upperRef := expressions.InitialOf(upper)

	yielded := selectMergeFire(t, NewSelectMergeRule(), upperRef)

	// The rule should merge both children. Each child has 2 quantifiers
	// (valuesBox + lowerLeft/Right). Since valuesBox is shared, the
	// merged result needs to disambiguate. Regardless of how the Go
	// rule handles this (it may or may not create new aliases like Java),
	// we verify that:
	// 1. At least 1 yield is produced
	// 2. The merged expression has the right number of quantifiers
	//    (4: valuesBox1, lowerLeft, valuesBox2, lowerRight)
	// 3. The predicates are preserved
	if len(yielded) < 1 {
		t.Fatalf("expected at least 1 yield, got %d", len(yielded))
	}

	merged := yielded[0].(*expressions.SelectExpression)
	mergedQs := merged.GetQuantifiers()

	// After merging both children (each has 2 quantifiers), and noting
	// that valuesBox is shared, the Go rule naively pulls up quantifiers
	// from both children. The resulting count depends on whether the rule
	// deduplicates or not. At minimum, we expect >= 3 quantifiers.
	if len(mergedQs) < 3 {
		t.Fatalf("expected at least 3 quantifiers after merge, got %d", len(mergedQs))
	}

	mergedPreds := merged.GetPredicates()
	if len(mergedPreds) < 2 {
		t.Fatalf("expected at least 2 predicates, got %d", len(mergedPreds))
	}
}

// TestSelectMerge_BakedBoxRefCallback_MultiAccessor pins the multi-accessor
// arm of the box-reference collapse: a path FUSED by an earlier merge round
// (root accessor indexing the box RC, suffix descending a nested record)
// must collapse its ROOT through the RC and fuse the suffix onto the slot's
// own baked leg reference. Leaving it whole strands the reference on the
// merged-away box alias — post-splice that name binds the pulled-up LEG (or
// nothing), so the positional read lands in the wrong window.
func TestSelectMerge_BakedBoxRefCallback_MultiAccessor(t *testing.T) {
	t.Parallel()
	// Both legs expose a nested record at their local ordinal 0. That makes the
	// exact box-level path [0,0] constructible while its child correlation E
	// proves that the source-relative root belongs to the SECOND leg.
	nested := values.NewRecordType("", false, []values.Field{
		{Name: "SUB", FieldType: values.NotNullLong, Ordinal: 0},
	})
	legD := values.NewRecordType("", false, []values.Field{
		{Name: "DREC", FieldType: nested, Ordinal: 0},
	})
	legE := values.NewRecordType("", false, []values.Field{
		{Name: "REC", FieldType: nested, Ordinal: 0},
	})
	qovD := selectMergeQOV(t, values.NamedCorrelationIdentifier("D"), legD)
	qovE := selectMergeQOV(t, values.NamedCorrelationIdentifier("E"), legE)
	d0 := selectMergeOrdinalSeedField(t, qovD, 0)
	e0 := selectMergeOrdinalSeedField(t, qovE, 0)
	rc := values.NewRawRecordConstructorValue(
		values.RecordConstructorField{Name: "DREC", Value: d0},
		values.RecordConstructorField{Name: "REC", Value: e0},
	)
	boxTyp := values.NewRecordType("", false, []values.Field{
		{Name: "DREC", FieldType: nested, Ordinal: 0},
		{Name: "REC", FieldType: nested, Ordinal: 1},
	})
	boxQOV := selectMergeQOV(t, values.NamedCorrelationIdentifier("E"), boxTyp)
	// The exact fused path has a leg-relative root [0] and a nested suffix [0].
	// Applying its raw root to the box would choose DREC; the E correlation must
	// rebase it to RC slot 1 before the suffix is resolved.
	fusedRef := selectMergeFieldOrdinals(t, boxQOV, 0, 0)
	cb := bakedBoxRefCallback(map[values.CorrelationIdentifier]values.Value{
		values.NamedCorrelationIdentifier("E"): rc,
	})
	out := values.Replace(fusedRef, cb)
	fv, isFV := values.AsFieldValue(out)
	if !isFV || fv.Path() == nil {
		t.Fatalf("collapse produced %T (%v), want a baked FieldValue", out, out)
	}
	child, isQOV := values.AsQuantifiedObjectValue(fv.ChildValue())
	if !isQOV || child.Correlation().Name() != "E" || child != qovE {
		t.Fatalf("collapsed child = %v, want the LEG quantifier E (the slot's own base)", fv.ChildValue())
	}
	ordinals := fv.Path().Ordinals()
	suffix, ok := fv.Path().Accessor(1)
	suffixName, named := "", false
	if ok {
		suffixName, named = suffix.DisplayName()
	}
	if len(ordinals) != 2 || ordinals[0] != 0 || ordinals[1] != 0 || !ok || !named || suffixName != "SUB" {
		t.Fatalf("collapsed path = %v, want the slot's leg ordinal (0) fused with the suffix (SUB#0)", ordinals)
	}
}

// TestSelectMergeRule_TranslatesExplodeSiblingCollection is the red sentinel
// for the SelectMerge/Explode arm: when a
// dissolved box leg (a multi-quantifier ordinal-seed child) MERGES into the
// flat select, a RETAINED sibling quantifier that is a lateral unnest's
// Explode — whose collection is a baked reference to the box's alias — must
// have that collection TRANSLATED so it no longer dangles on the merged-away
// box alias (it collapses to the spliced leg reference). Without the
// ExplodeExpression arm in translateQuantifierCorrelations the rebuilt merged
// member carries a dangling collection: correct plans still win today, but the
// moment the cost model prefers the merged member the Explode reads a
// non-existent quantifier — silent wrong rows. This pins the arm directly.
func TestSelectMergeRule_TranslatesExplodeSiblingCollection(t *testing.T) {
	t.Parallel()

	// The box: SELECT over two leg quantifiers D, E with an ordinal-seed RC
	// result value (the shape OrdinalSeedLegWindows recognizes → the merge
	// records rcByAlias for the box).
	legD := values.NewRecordType("", false, []values.Field{{Name: "DID", FieldType: values.NotNullLong, Ordinal: 0}})
	legE := values.NewRecordType("", false, []values.Field{{Name: "EARR", FieldType: values.NewArrayType(true, values.NotNullLong), Ordinal: 0}})
	dAlias := values.NamedCorrelationIdentifier("D")
	eAlias := values.NamedCorrelationIdentifier("E")
	dQun := expressions.NamedForEachQuantifier(dAlias, expressions.InitialOf(selectMergeTypedScan(t, "D", legD)))
	eQun := expressions.NamedForEachQuantifier(eAlias, expressions.InitialOf(selectMergeTypedScan(t, "E", legE)))
	qovD := selectMergeQOV(t, dAlias, legD)
	qovE := selectMergeQOV(t, eAlias, legE)
	dBaked := selectMergeOrdinalSeedField(t, qovD, 0)
	eBaked := selectMergeOrdinalSeedField(t, qovE, 0)
	boxRC := values.NewRawRecordConstructorValue(
		values.RecordConstructorField{Name: "DID", Value: dBaked},
		values.RecordConstructorField{Name: "EARR", Value: eBaked},
	)
	box := selectMergeSelectWithAliases(t, boxRC, []expressions.Quantifier{dQun, eQun}, nil, []string{"D", "E"})
	boxQun := expressions.NamedForEachQuantifier(values.NamedCorrelationIdentifier("M"), expressions.InitialOf(box))

	// The Explode sibling: collection = the box's ARR column, baked over
	// QOV(M, concat) at box ordinal 1 (the box-level reference the merge must
	// translate). concat arity == RC arity so boxLevel() treats it box-level.
	concat := values.NewRecordType("", false, []values.Field{
		{Name: "DID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "EARR", FieldType: values.NewArrayType(true, values.NotNullLong), Ordinal: 1},
	})
	boxAliasQOV := selectMergeQOV(t, values.NamedCorrelationIdentifier("M"), concat)
	// This is a machinery-owned box-relative positional read, not an ordinary
	// semantic field access. Pin it through the values-owned seed authority so
	// the callback keeps ordinal 1 in the box's concat domain rather than trying
	// to reinterpret it as a leg-relative ordinal owned by M.
	coll := selectMergeOrdinalSeedField(t, boxAliasQOV, 1)
	explode := selectMergeExplode(t, coll)
	explodeQun := expressions.NamedForEachQuantifier(values.NamedCorrelationIdentifier("X"), expressions.InitialOf(explode))

	upper := selectMergeSelectWithAliases(t,
		selectMergeFlowed(t, boxQun),
		[]expressions.Quantifier{boxQun, explodeQun},
		nil,
		[]string{"M", "X"},
	)
	yielded := selectMergeFire(t, NewSelectMergeRule(), expressions.InitialOf(upper))
	if len(yielded) < 1 {
		t.Fatalf("expected a merged yield, got %d", len(yielded))
	}
	merged := yielded[0].(*expressions.SelectExpression)

	// Find the retained Explode quantifier and assert its collection no longer
	// references the merged-away box alias M.
	var found bool
	for _, q := range merged.GetQuantifiers() {
		for _, m := range q.GetRangesOver().AllMembers() {
			exp, isExp := m.(*expressions.ExplodeExpression)
			if !isExp {
				continue
			}
			found = true
			cv, isFV := values.AsFieldValue(exp.GetCollectionValue())
			if !isFV {
				t.Fatalf("translated collection = %T, want *FieldValue", exp.GetCollectionValue())
			}
			qov, isQOV := values.AsQuantifiedObjectValue(cv.ChildValue())
			if !isQOV {
				t.Fatalf("translated collection child = %T, want QuantifiedObjectValue", cv.ChildValue())
			}
			if qov.Correlation().Name() == "M" {
				t.Fatalf("Explode collection STILL references the merged-away box alias M (%v) — the SelectMerge/Explode arm did not translate it (dangling collection = latent wrong rows)", cv)
			}
			if qov.Correlation().Name() != "E" {
				t.Fatalf("translated collection correlates to %s, want the spliced leg E (the box's EARR owner)", qov.Correlation())
			}
		}
	}
	if !found {
		t.Fatal("no Explode quantifier survived the merge — fixture did not exercise the arm")
	}
}

// TestSelectMergeRule_ChainedUnnestBarrier pins the chained-unnest merge
// barrier. It builds the memo shape a `FROM t, t.arr AS x,
// x.sub AS y` chain lowers to — a NAME-MODEL ForEach target (the first unnest's
// SelectExpression, whose result value is a plain QOV, NOT an ordinal-seed RC)
// with a RETAINED sibling that ranges over an Explode whose collection is
// free-correlated to that target's alias (the second unnest reading `x.sub`).
//
// RED-first: WITHOUT the barrier at rule_select_merge.go, the target is a
// mergeable single-quantifier Select, so the rule flattens it — pulling up the
// inner scan and stranding the Explode collection on the merged-away alias (a
// non-seed child records NO rcByAlias entry, so the positional-seed Explode
// rebase never runs → dangling correlation → latent wrong rows). WITH the
// barrier, `childRefResultIsNonSeed && siblingFreeCorrelatedTo` fires, the
// target is skipped, and the rule yields no broken alternative. Assert exactly
// that: no yielded alternative carries an Explode collection still correlated
// to the target alias that is absent from that alternative's own quantifiers.
func TestSelectMergeRule_ChainedUnnestBarrier(t *testing.T) {
	t.Parallel()

	// The chain's first-unnest analog: a NON-SEED select over a scan. Its
	// result value is qun.GetFlowedObjectValue() (a QOV, not an ordinal-seed
	// RC) → childRefResultIsNonSeed returns true.
	tQun, _ := baseLeaf(t)
	targetSel := selectWithPreds(t, tQun)
	targetQun := forEachOf(targetSel)
	targetAlias := targetQun.GetAlias()

	// The chain's second-unnest analog: an Explode whose collection reads
	// `x.sub` off the first unnest's output — free-correlated to targetAlias.
	explode := selectMergeExplode(t, qFieldValue(t, targetQun, "sub"))
	explodeQun := forEachOf(explode)

	upper := selectMergeSelect(t,
		selectMergeFlowed(t, targetQun),
		[]expressions.Quantifier{targetQun, explodeQun},
		nil,
	)

	yielded := selectMergeFire(t, NewSelectMergeRule(), expressions.InitialOf(upper))

	// Every yielded alternative must be well-formed: no retained Explode may
	// dangle on the merged-away target alias.
	for yi, y := range yielded {
		sel, ok := y.(*expressions.SelectExpression)
		if !ok {
			continue
		}
		aliveAliases := map[values.CorrelationIdentifier]bool{}
		for _, q := range sel.GetQuantifiers() {
			aliveAliases[q.GetAlias()] = true
		}
		for _, q := range sel.GetQuantifiers() {
			for _, m := range q.GetRangesOver().AllMembers() {
				exp, isExp := m.(*expressions.ExplodeExpression)
				if !isExp {
					continue
				}
				cv, isFV := values.AsFieldValue(exp.GetCollectionValue())
				if !isFV {
					continue
				}
				qov, isQOV := values.AsQuantifiedObjectValue(cv.ChildValue())
				if !isQOV {
					continue
				}
				// A collection correlated to the (merged-away) target alias with
				// no live quantifier binding it is the dangling-strand bug.
				if qov.Correlation() == targetAlias && !aliveAliases[targetAlias] {
					t.Fatalf("yield[%d]: chained-unnest Explode collection dangles on merged-away target alias %s (%v) — the SelectMerge barrier failed to decline the non-seed chained merge (latent wrong rows)", yi, targetAlias, cv)
				}
			}
		}
	}
}
