package properties

import (
	"fmt"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func propertyTestFlowedType() values.Type {
	return exactRecord(values.Field{Name: "value", FieldType: values.NotNullLong})
}

func propertyField(t testing.TB, name string, fieldType values.Type) values.Value {
	t.Helper()
	return propertyFieldIn(t, exactRecord(values.Field{Name: name, FieldType: fieldType}), name)
}

func propertyFieldIn(t testing.TB, row values.Type, name string) values.Value {
	t.Helper()
	child := mustQOV(t, values.NamedCorrelationIdentifier("properties_test_row"), row)
	return propertyFieldFrom(t, child, name)
}

func propertyFieldFrom(t testing.TB, child values.Value, name string) values.Value {
	t.Helper()
	request, err := values.FieldByName(name)
	if err != nil {
		t.Fatalf("FieldByName(%q): %v", name, err)
	}
	field, err := values.ResolveFieldAccess(child, []values.FieldRequest{request})
	if err != nil {
		t.Fatalf("ResolveFieldAccess(%q): %v", name, err)
	}
	return field
}

func propertyFieldFromOrdinal(t testing.TB, child values.Value, ordinal int) values.Value {
	t.Helper()
	field, err := values.ResolveFieldOrdinals(child, []int{ordinal})
	if err != nil {
		t.Fatalf("ResolveFieldOrdinals(%d): %v", ordinal, err)
	}
	return field
}

func propertyFieldAt(t testing.TB, name string, ordinal int, fieldType values.Type) values.Value {
	t.Helper()
	fields := make([]values.Field, ordinal+1)
	for i := range fields {
		fields[i] = values.Field{Name: fmt.Sprintf("unused_%d", i), FieldType: values.NullableLong}
	}
	fields[ordinal] = values.Field{Name: name, FieldType: fieldType}
	child := mustQOV(t, values.NamedCorrelationIdentifier("properties_test_row"), exactRecord(fields...))
	field, err := values.ResolveFieldOrdinals(child, []int{ordinal})
	if err != nil {
		t.Fatalf("ResolveFieldOrdinals(%q, %d): %v", name, ordinal, err)
	}
	return field
}

func mustFullUnorderedScanExpression(
	t testing.TB,
	recordTypes []string,
	flowedType values.Type,
) *expressions.FullUnorderedScanExpression {
	t.Helper()
	expression, err := expressions.NewFullUnorderedScanExpression(recordTypes, flowedType)
	if err != nil {
		t.Fatalf("NewFullUnorderedScanExpression: %v", err)
	}
	return expression
}

func mustLogicalFilterExpression(
	t testing.TB,
	queryPredicates []predicates.QueryPredicate,
	inner expressions.Quantifier,
) *expressions.LogicalFilterExpression {
	t.Helper()
	expression, err := expressions.NewLogicalFilterExpression(queryPredicates, inner)
	if err != nil {
		t.Fatalf("NewLogicalFilterExpression: %v", err)
	}
	return expression
}

func mustLogicalTypeFilterExpression(
	t testing.TB,
	recordTypes []string,
	inner expressions.Quantifier,
) *expressions.LogicalTypeFilterExpression {
	t.Helper()
	expression, err := expressions.NewLogicalTypeFilterExpression(recordTypes, inner)
	if err != nil {
		t.Fatalf("NewLogicalTypeFilterExpression: %v", err)
	}
	return expression
}

func mustLogicalSortExpression(
	t testing.TB,
	sortKeys []expressions.SortKey,
	inner expressions.Quantifier,
) *expressions.LogicalSortExpression {
	t.Helper()
	expression, err := expressions.NewLogicalSortExpression(sortKeys, inner)
	if err != nil {
		t.Fatalf("NewLogicalSortExpression: %v", err)
	}
	return expression
}

func mustLogicalDistinctExpression(
	t testing.TB,
	inner expressions.Quantifier,
) *expressions.LogicalDistinctExpression {
	t.Helper()
	expression, err := expressions.NewLogicalDistinctExpression(inner)
	if err != nil {
		t.Fatalf("NewLogicalDistinctExpression: %v", err)
	}
	return expression
}

func mustLogicalUnionExpression(
	t testing.TB,
	quantifiers []expressions.Quantifier,
) *expressions.LogicalUnionExpression {
	t.Helper()
	expression, err := expressions.NewLogicalUnionExpression(quantifiers)
	if err != nil {
		t.Fatalf("NewLogicalUnionExpression: %v", err)
	}
	return expression
}

func mustLogicalIntersectionExpression(
	t testing.TB,
	quantifiers []expressions.Quantifier,
	comparisonKeyValues []values.Value,
) *expressions.LogicalIntersectionExpression {
	t.Helper()
	expression, err := expressions.NewLogicalIntersectionExpression(quantifiers, comparisonKeyValues)
	if err != nil {
		t.Fatalf("NewLogicalIntersectionExpression: %v", err)
	}
	return expression
}

func mustLogicalProjectionExpression(
	t testing.TB,
	projectedValues []values.Value,
	inner expressions.Quantifier,
) *expressions.LogicalProjectionExpression {
	t.Helper()
	expression, err := expressions.NewLogicalProjectionExpression(projectedValues, inner)
	if err != nil {
		t.Fatalf("NewLogicalProjectionExpression: %v", err)
	}
	return expression
}

func mustLogicalUniqueExpression(
	t testing.TB,
	inner expressions.Quantifier,
) *expressions.LogicalUniqueExpression {
	t.Helper()
	expression, err := expressions.NewLogicalUniqueExpression(inner)
	if err != nil {
		t.Fatalf("NewLogicalUniqueExpression: %v", err)
	}
	return expression
}

func mustInsertExpression(
	t testing.TB,
	inner expressions.Quantifier,
	targetRecordType string,
	targetType values.Type,
) *expressions.InsertExpression {
	t.Helper()
	expression, err := expressions.NewInsertExpression(inner, targetRecordType, targetType)
	if err != nil {
		t.Fatalf("NewInsertExpression: %v", err)
	}
	return expression
}

func mustDeleteExpression(
	t testing.TB,
	inner expressions.Quantifier,
	targetRecordType string,
) *expressions.DeleteExpression {
	t.Helper()
	expression, err := expressions.NewDeleteExpression(inner, targetRecordType)
	if err != nil {
		t.Fatalf("NewDeleteExpression: %v", err)
	}
	return expression
}

func mustQOV(t testing.TB, correlation values.CorrelationIdentifier, flowed values.Type) values.QuantifiedObjectValue {
	t.Helper()
	qov, err := values.NewQuantifiedObjectValue(correlation, flowed)
	if err != nil {
		t.Fatalf("NewQuantifiedObjectValue(%q, %v): %v", correlation, flowed, err)
	}
	return qov
}

func exactRecord(fields ...values.Field) values.Type {
	// RecordConstructorValue.Type() is the authority most ordering translation
	// fixtures cross. A successfully constructed row is non-null even when its
	// individual fields are nullable, so expected output owners must carry that
	// same exact container nullability.
	return values.NewRecordType("", false, fields)
}

func mustPullUpThroughValue(
	t testing.TB,
	ordering *RichOrdering,
	resultValue values.Value,
	alias values.CorrelationIdentifier,
) *RichOrdering {
	t.Helper()
	pulled, err := ordering.PullUpThroughValue(resultValue, alias)
	if err != nil {
		t.Fatalf("PullUpThroughValue: %v", err)
	}
	return pulled
}
