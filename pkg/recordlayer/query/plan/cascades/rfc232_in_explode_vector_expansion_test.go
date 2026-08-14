package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestInComparisonToExplodePublishesExactFlowedValues(t *testing.T) {
	t.Parallel()

	rowType := values.NewRecordType("T", false, []values.Field{
		{Name: "ID", Ordinal: 0, FieldType: values.NotNullLong},
	})
	scan, err := expressions.NewFullUnorderedScanExpression([]string{"T"}, rowType)
	if err != nil {
		t.Fatalf("NewFullUnorderedScanExpression: %v", err)
	}
	baseQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	base, err := baseQ.RequireFlowedObjectValue()
	if err != nil {
		t.Fatalf("RequireFlowedObjectValue: %v", err)
	}
	idRequest, err := values.FieldByName("ID")
	if err != nil {
		t.Fatalf("FieldByName: %v", err)
	}
	id, err := values.ResolveFieldAccess(base, []values.FieldRequest{idRequest})
	if err != nil {
		t.Fatalf("ResolveFieldAccess: %v", err)
	}
	inPredicate := predicates.NewComparisonPredicate(id, predicates.Comparison{
		Type: predicates.ComparisonIn,
		Operand: &values.ConstantValue{
			Value: []any{int64(1), int64(2)},
			Typ:   values.UnknownType,
		},
	})
	filter, err := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{inPredicate},
		baseQ,
	)
	if err != nil {
		t.Fatalf("NewLogicalFilterExpression: %v", err)
	}

	yielded, err := FireExpressionRuleWithMemo(
		NewInComparisonToExplodeRule(),
		expressions.InitialOf(filter),
		EmptyPlanContext(),
		nil,
	)
	if err != nil {
		t.Fatalf("FireExpressionRuleWithMemo: %v", err)
	}
	if len(yielded) != 1 {
		t.Fatalf("yield count = %d, want 1", len(yielded))
	}
	selectExpr, ok := yielded[0].(*expressions.SelectExpression)
	if !ok {
		t.Fatalf("yield = %T, want *expressions.SelectExpression", yielded[0])
	}
	result, ok := values.AsQuantifiedObjectValue(selectExpr.GetResultValue())
	if !ok {
		t.Fatalf("result value = %T, want admitted QuantifiedObjectValue", selectExpr.GetResultValue())
	}
	if !result.FlowedType().Equals(rowType) {
		t.Fatalf("result flowed type = %v, want %v", result.FlowedType(), rowType)
	}

	quantifiers := selectExpr.GetQuantifiers()
	if len(quantifiers) != 2 {
		t.Fatalf("quantifier count = %d, want 2", len(quantifiers))
	}
	explode, ok := quantifiers[1].GetRangesOver().Get().(*expressions.ExplodeExpression)
	if !ok {
		t.Fatalf("explode member = %T, want *expressions.ExplodeExpression", quantifiers[1].GetRangesOver().Get())
	}
	if !explode.GetElementType().Equals(values.NotNullLong) {
		t.Fatalf("explode element type = %v, want %v", explode.GetElementType(), values.NotNullLong)
	}

	innerFilter, ok := quantifiers[0].GetRangesOver().Get().(*expressions.LogicalFilterExpression)
	if !ok {
		t.Fatalf("inner member = %T, want *expressions.LogicalFilterExpression", quantifiers[0].GetRangesOver().Get())
	}
	equality, ok := innerFilter.GetPredicates()[0].(*predicates.ComparisonPredicate)
	if !ok {
		t.Fatalf("inner predicate = %T, want *predicates.ComparisonPredicate", innerFilter.GetPredicates()[0])
	}
	exploded, ok := values.AsQuantifiedObjectValue(equality.Comparison.Operand)
	if !ok {
		t.Fatalf("equality RHS = %T, want admitted QuantifiedObjectValue", equality.Comparison.Operand)
	}
	if !exploded.FlowedType().Equals(values.NotNullLong) {
		t.Fatalf("equality RHS type = %v, want %v", exploded.FlowedType(), values.NotNullLong)
	}
	if exploded.Correlation() != quantifiers[1].GetAlias() {
		t.Fatalf("equality RHS correlation = %v, want %v", exploded.Correlation(), quantifiers[1].GetAlias())
	}
}

func TestExactInExplodeElementTypeDeclinesUnresolvedAndPreservesNullability(t *testing.T) {
	t.Parallel()

	unresolved := predicates.NewComparisonPredicate(
		&values.ConstantValue{Value: int64(1), Typ: values.UnknownType},
		predicates.Comparison{
			Type:    predicates.ComparisonIn,
			Operand: &values.ConstantValue{Value: []any{int64(1), int64(2)}, Typ: values.UnknownType},
		},
	)
	if got, ok := exactInExplodeElementType(unresolved, []any{int64(1), int64(2)}); ok || got != nil {
		t.Fatalf("unresolved element type = (%v, %v), want declined", got, ok)
	}

	typed := predicates.NewComparisonPredicate(
		&values.ConstantValue{Value: "lhs", Typ: values.NotNullString},
		predicates.Comparison{
			Type: predicates.ComparisonIn,
			Operand: &values.ConstantValue{
				Value: []any{"rhs", nil},
				Typ:   values.NewArrayType(false, values.NotNullString),
			},
		},
	)
	got, ok := exactInExplodeElementType(typed, []any{"rhs", nil})
	if !ok {
		t.Fatal("exact array element type unexpectedly declined")
	}
	if !got.Equals(values.NullableString) {
		t.Fatalf("element type = %v, want nullable STRING", got)
	}
}

func TestExpandVectorIndexResolvesExactOrdinalFields(t *testing.T) {
	t.Parallel()

	vectorType := values.NewArrayType(false, values.NotNullDouble)
	rowType := values.NewRecordType("T", false, []values.Field{
		{Name: "PART", Ordinal: 0, FieldType: values.NotNullString},
		{Name: "EMBEDDING", Ordinal: 1, FieldType: vectorType},
	})
	candidate := NewVectorIndexScanMatchCandidate(
		"vector_idx",
		[]string{"T"},
		[]string{"PART", "EMBEDDING"},
		1,
		values.DistanceCosine,
		rowType,
		false,
		nil,
	)

	traversal := ExpandVectorIndex(candidate)
	if traversal == nil {
		t.Fatal("ExpandVectorIndex unexpectedly declined an exact candidate")
	}
	sortExpr, ok := traversal.GetRootReference().Get().(*expressions.MatchableSortExpression)
	if !ok {
		t.Fatalf("root = %T, want *expressions.MatchableSortExpression", traversal.GetRootReference().Get())
	}
	selectExpr, ok := sortExpr.GetInner().GetRangesOver().Get().(*expressions.SelectExpression)
	if !ok {
		t.Fatalf("sort child = %T, want *expressions.SelectExpression", sortExpr.GetInner().GetRangesOver().Get())
	}
	queryPredicates := selectExpr.GetPredicates()
	if len(queryPredicates) != 2 {
		t.Fatalf("placeholder count = %d, want 2", len(queryPredicates))
	}
	partitionPlaceholder, ok := queryPredicates[0].(*predicates.Placeholder)
	if !ok {
		t.Fatalf("predicate[0] = %T, want *predicates.Placeholder", queryPredicates[0])
	}
	assertRFC232ResolvedField(t, partitionPlaceholder.GetValue(), 0, values.NotNullString)

	distancePlaceholder, ok := queryPredicates[1].(*predicates.Placeholder)
	if !ok {
		t.Fatalf("predicate[1] = %T, want *predicates.Placeholder", queryPredicates[1])
	}
	distance, ok := distancePlaceholder.GetValue().(*values.CosineDistanceRowNumberValue)
	if !ok {
		t.Fatalf("distance placeholder value = %T, want *values.CosineDistanceRowNumberValue", distancePlaceholder.GetValue())
	}
	if len(distance.PartitioningValues) != 1 || len(distance.ArgumentValues) != 1 {
		t.Fatalf("distance children = (%d partition, %d argument), want (1, 1)", len(distance.PartitioningValues), len(distance.ArgumentValues))
	}
	assertRFC232ResolvedField(t, distance.PartitioningValues[0], 0, values.NotNullString)
	assertRFC232ResolvedField(t, distance.ArgumentValues[0], 1, vectorType)
}

func TestExpandVectorIndexDeclinesMissingTypedColumn(t *testing.T) {
	t.Parallel()

	rowType := values.NewRecordType("T", false, []values.Field{
		{Name: "PART", Ordinal: 0, FieldType: values.NotNullString},
		{Name: "EMBEDDING", Ordinal: 1, FieldType: values.NewArrayType(false, values.NotNullDouble)},
	})
	candidate := NewVectorIndexScanMatchCandidate(
		"bad_vector_idx",
		[]string{"T"},
		[]string{"PART", "NOT_A_COLUMN"},
		1,
		values.DistanceEuclidean,
		rowType,
		false,
		nil,
	)
	if traversal := ExpandVectorIndex(candidate); traversal != nil {
		t.Fatal("ExpandVectorIndex admitted a vector column absent from the exact row type")
	}
}

func assertRFC232ResolvedField(t testing.TB, value values.Value, ordinal int, typ values.Type) {
	t.Helper()
	field, ok := values.AsFieldValue(value)
	if !ok {
		t.Fatalf("value = %T, want admitted FieldValue", value)
	}
	path := field.Path()
	if path == nil {
		t.Fatal("admitted FieldValue returned a nil path")
	}
	ordinals := path.Ordinals()
	if path.Len() != 1 || len(ordinals) != 1 || ordinals[0] != ordinal {
		t.Fatalf("field ordinals = %v, want [%d]", ordinals, ordinal)
	}
	if !field.ResultType().Equals(typ) {
		t.Fatalf("field result type = %v, want %v", field.ResultType(), typ)
	}
	if _, ok := values.AsQuantifiedObjectValue(field.ChildValue()); !ok {
		t.Fatalf("field child = %T, want admitted QuantifiedObjectValue", field.ChildValue())
	}
}
