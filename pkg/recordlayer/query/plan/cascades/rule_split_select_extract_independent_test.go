package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func splitSelectTestRowType() *values.RecordType {
	return values.NewRecordType("SPLIT_SELECT_ROW", false, []values.Field{{
		Name:      "ID",
		FieldType: values.NullableLong,
		Ordinal:   0,
	}})
}

func mustSplitSelectScan(t testing.TB) *expressions.FullUnorderedScanExpression {
	t.Helper()
	scan, err := expressions.NewFullUnorderedScanExpression([]string{"T"}, splitSelectTestRowType())
	return mustConstruct(t, scan, err)
}

func mustSplitSelectInitial(
	t testing.TB,
	expression expressions.RelationalExpression,
) *expressions.Reference {
	t.Helper()
	reference, err := InitialOf(expression)
	return mustConstruct(t, reference, err)
}

func mustSplitSelectFlowed(
	t testing.TB,
	quantifier expressions.Quantifier,
) values.QuantifiedObjectValue {
	t.Helper()
	flowed, err := quantifier.RequireFlowedObjectValue()
	return mustConstruct(t, flowed, err)
}

func mustSplitSelectField(
	t testing.TB,
	root values.Value,
	name string,
) values.Value {
	t.Helper()
	request, err := values.FieldByName(name)
	request = mustConstruct(t, request, err)
	field, err := values.ResolveFieldAccess(root, []values.FieldRequest{request})
	return mustConstruct(t, field, err)
}

func mustSplitSelectExplode(t testing.TB) *expressions.ExplodeExpression {
	t.Helper()
	collection := &values.ConstantValue{
		Value: []any{int64(1), int64(2)},
		Typ:   values.NewArrayType(false, values.NotNullLong),
	}
	explode, err := expressions.NewExplodeExpression(collection)
	return mustConstruct(t, explode, err)
}

func mustSplitSelect(
	t testing.TB,
	result values.Value,
	quantifiers []expressions.Quantifier,
	queryPredicates []predicates.QueryPredicate,
) *expressions.SelectExpression {
	t.Helper()
	selectExpression, err := expressions.NewSelectExpression(result, quantifiers, queryPredicates)
	return mustConstruct(t, selectExpression, err)
}

func mustSplitSelectWithJoinType(
	t testing.TB,
	result values.Value,
	quantifiers []expressions.Quantifier,
	queryPredicates []predicates.QueryPredicate,
	joinType expressions.JoinType,
) *expressions.SelectExpression {
	t.Helper()
	selectExpression, err := expressions.NewSelectExpressionWithJoinType(
		result, quantifiers, queryPredicates, nil, joinType)
	return mustConstruct(t, selectExpression, err)
}

func splitSelectIDPredicate(
	t testing.TB,
	scanRow values.Value,
) predicates.QueryPredicate {
	t.Helper()
	return predicates.NewComparisonPredicate(
		mustSplitSelectField(t, scanRow, "ID"),
		predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, int64(0)),
	)
}

func TestSplitSelectExtractIndependentQuantifiersRule_Fires(t *testing.T) {
	t.Parallel()

	scan := mustSplitSelectScan(t)
	scanRef := mustSplitSelectInitial(t, scan)
	scanQ := expressions.ForEachQuantifier(scanRef)
	scanRow := mustSplitSelectFlowed(t, scanQ)

	explode := mustSplitSelectExplode(t)
	explodeRef := mustSplitSelectInitial(t, explode)
	explodeQ := expressions.ForEachQuantifier(explodeRef)

	// The predicate makes the SELECT non-trivial, but it belongs entirely to
	// the scan leg. The independent explode leg is therefore eligible for
	// extraction into the outer SELECT.
	predicate := splitSelectIDPredicate(t, scanRow)
	selectExpression := mustSplitSelect(t,
		scanRow,
		[]expressions.Quantifier{scanQ, explodeQ},
		[]predicates.QueryPredicate{predicate},
	)

	// Invoke one matched binding directly. FireExpressionRule also exercises
	// the Select ChildrenAsSet permutation and therefore reports both equivalent
	// quantifier orders; this isolates one rule firing.
	reference := mustSplitSelectInitial(t, selectExpression)
	rule := NewSplitSelectExtractIndependentQuantifiersRule()
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), selectExpression)
	if len(bindings) != 1 {
		t.Fatalf("matcher produced %d bindings, want one", len(bindings))
	}
	call := NewExpressionRuleCall(reference, bindings[0], EmptyPlanContext())
	rule.OnMatch(call)
	if err := call.Err(); err != nil {
		t.Fatalf("SplitSelectExtractIndependentQuantifiersRule.OnMatch: %v", err)
	}
	yielded := call.Yielded()
	if len(yielded) != 1 {
		t.Fatalf("yielded %d split SELECTs, want one", len(yielded))
	}

	outer, ok := yielded[0].(*expressions.SelectExpression)
	if !ok {
		t.Fatalf("yielded %T, want *expressions.SelectExpression", yielded[0])
	}
	// Standalone OnMatch does not run the driver's atomic commit, so explicitly
	// admit the result to keep this direct-binding test on the checked path.
	_ = mustSplitSelectInitial(t, outer)
	if outer.GetJoinType() != expressions.JoinInner || len(outer.GetPredicates()) != 0 {
		t.Fatalf("outer SELECT = {join %v, predicates %d}, want inner join with no predicates",
			outer.GetJoinType(), len(outer.GetPredicates()))
	}
	outerQuantifiers := outer.GetQuantifiers()
	if len(outerQuantifiers) != 2 {
		t.Fatalf("outer SELECT has %d quantifiers, want explode + lower SELECT", len(outerQuantifiers))
	}
	if outerQuantifiers[0].GetRangesOver() != explodeRef {
		t.Fatal("independent explode quantifier was not extracted to the outer SELECT")
	}
	outerResult, ok := values.AsQuantifiedObjectValue(outer.GetResultValue())
	if !ok {
		t.Fatalf("outer result = %T, want exact QuantifiedObjectValue over the lower SELECT", outer.GetResultValue())
	}
	if outerResult.Correlation() != outerQuantifiers[1].GetAlias() {
		t.Fatalf("outer result alias = %s, want lower SELECT alias %s",
			outerResult.Correlation().Name(), outerQuantifiers[1].GetAlias().Name())
	}
	if !outerResult.FlowedType().Equals(splitSelectTestRowType()) {
		t.Fatalf("outer result type = %v, want %v", outerResult.FlowedType(), splitSelectTestRowType())
	}

	lowerRef := outerQuantifiers[1].GetRangesOver()
	if lowerRef == nil {
		t.Fatal("outer SELECT has no lower SELECT reference")
	}
	lower, ok := lowerRef.Get().(*expressions.SelectExpression)
	if !ok {
		t.Fatalf("lower member is %T, want *expressions.SelectExpression", lowerRef.Get())
	}
	lowerQuantifiers := lower.GetQuantifiers()
	if len(lowerQuantifiers) != 1 || lowerQuantifiers[0].GetRangesOver() != scanRef {
		t.Fatalf("lower SELECT quantifiers = %#v, want only the scan leg", lowerQuantifiers)
	}
	lowerPredicates := lower.GetPredicates()
	if len(lowerPredicates) != 1 || lowerPredicates[0] != predicate {
		t.Fatalf("lower SELECT predicates = %v, want the original predicate instance", lowerPredicates)
	}
	correlated := predicates.GetCorrelatedToOfPredicate(lowerPredicates[0])
	if len(correlated) != 1 {
		t.Fatalf("lower predicate correlations = %v, want only scan alias %q", correlated, scanQ.GetAlias().Name())
	}
	if _, ok := correlated[scanQ.GetAlias()]; !ok {
		t.Fatalf("lower predicate correlations = %v, want scan alias %q", correlated, scanQ.GetAlias().Name())
	}
	if !values.ValuesStructurallyEqual(lower.GetResultValue(), scanRow) {
		t.Fatalf("lower result = %s, want original result %s",
			values.ExplainValue(lower.GetResultValue()), values.ExplainValue(scanRow))
	}
}

func TestSplitSelectExtractIndependentQuantifiersRule_RecordExplodeStaysRelational(t *testing.T) {
	t.Parallel()

	scanQ := expressions.ForEachQuantifier(mustSplitSelectInitial(t, mustSplitSelectScan(t)))
	scanRow := mustSplitSelectFlowed(t, scanQ)
	row := values.NewRawRecordConstructorValue(
		values.RecordConstructorField{
			Name:  "ID",
			Value: &values.ConstantValue{Typ: values.NotNullInt, Value: int32(1)},
		},
		values.RecordConstructorField{
			Name: "ARR",
			Value: values.NewArrayConstructorValue(values.NotNullInt, []values.Value{
				&values.ConstantValue{Typ: values.NotNullInt, Value: int32(101)},
			}),
		},
	)
	rows := values.NewArrayConstructorValue(row.Type(), []values.Value{row})
	originalElementType := rows.ElementType
	originalElement := rows.Elements[0]
	recordExplode, err := expressions.NewExplodeExpression(rows)
	recordExplode = mustConstruct(t, recordExplode, err)
	recordQ := expressions.ForEachQuantifier(mustSplitSelectInitial(t, recordExplode))
	selectExpression := mustSplitSelect(t,
		scanRow,
		[]expressions.Quantifier{scanQ, recordQ},
		[]predicates.QueryPredicate{splitSelectIDPredicate(t, scanRow)},
	)

	yielded := mustFireExpressionRule(t,
		NewSplitSelectExtractIndependentQuantifiersRule(), mustSplitSelectInitial(t, selectExpression))
	if len(yielded) != 0 {
		t.Fatalf("record-valued relation Explode yielded %d independent split(s), want zero", len(yielded))
	}
	if rows.ElementType != originalElementType || rows.Elements[0] != originalElement ||
		!row.Type().Equals(originalElementType) {
		t.Fatal("record-valued split classification mutated its source collection")
	}
}

// TestSplitSelectExtractIndependentQuantifiersRule_OuterJoinFailsClosed pins
// the ChildrenAsSet guard. Both halves this rule builds default to JoinInner,
// so splitting a JoinLeftOuter select would erase null-extension semantics.
func TestSplitSelectExtractIndependentQuantifiersRule_OuterJoinFailsClosed(t *testing.T) {
	t.Parallel()

	scanQ := expressions.ForEachQuantifier(mustSplitSelectInitial(t, mustSplitSelectScan(t)))
	scanRow := mustSplitSelectFlowed(t, scanQ)
	explodeQ := expressions.ForEachQuantifier(mustSplitSelectInitial(t, mustSplitSelectExplode(t)))
	selectExpression := mustSplitSelectWithJoinType(t,
		scanRow,
		[]expressions.Quantifier{scanQ, explodeQ},
		[]predicates.QueryPredicate{splitSelectIDPredicate(t, scanRow)},
		expressions.JoinLeftOuter,
	)

	yielded := mustFireExpressionRule(t,
		NewSplitSelectExtractIndependentQuantifiersRule(), mustSplitSelectInitial(t, selectExpression))
	if len(yielded) != 0 {
		t.Fatalf("LEFT OUTER select yielded %d split(s), want fail-closed zero", len(yielded))
	}
}

func TestSplitSelectExtractIndependentQuantifiersRule_StrictSingleFailsClosed(t *testing.T) {
	t.Parallel()

	scanRef := mustSplitSelectInitial(t, mustSplitSelectScan(t))
	scanAlias := values.NamedCorrelationIdentifier("STRICT")
	scanQ := expressions.NamedForEachStrictSingleQuantifier(scanAlias, scanRef)
	scanRow := mustSplitSelectFlowed(t, scanQ)
	explodeQ := expressions.ForEachQuantifier(mustSplitSelectInitial(t, mustSplitSelectExplode(t)))
	selectExpression := mustSplitSelect(t,
		scanRow,
		[]expressions.Quantifier{scanQ, explodeQ},
		[]predicates.QueryPredicate{splitSelectIDPredicate(t, scanRow)},
	)

	yielded := mustFireExpressionRule(t,
		NewSplitSelectExtractIndependentQuantifiersRule(), mustSplitSelectInitial(t, selectExpression))
	if len(yielded) != 0 {
		t.Fatalf("strict-single select yielded %d split(s), want zero", len(yielded))
	}
}
