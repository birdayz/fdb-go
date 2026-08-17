package expressions

import "fdb.dev/pkg/recordlayer/query/plan/cascades/values"

// mustExpression keeps successful constructor fixtures concise while every
// production constructor remains explicitly fallible. Negative-path tests call
// constructors directly and assert both the error and nil result.
func mustExpression[T any](value T, err error) T {
	if err != nil {
		panic(err)
	}
	return value
}

func mustQOV(correlation values.CorrelationIdentifier) values.QuantifiedObjectValue {
	return mustExpression(values.NewQuantifiedObjectValue(correlation, values.NotNullLong))
}

func testRecordType() *values.RecordType {
	return &values.RecordType{Fields: []values.Field{
		{Name: "ID", Ordinal: 0, FieldType: values.NotNullLong},
	}}
}

func mustResolvedField(child values.Value, names ...string) values.Value {
	requests := make([]values.FieldRequest, len(names))
	for i, name := range names {
		requests[i] = mustExpression(values.FieldByName(name))
	}
	return mustExpression(values.ResolveFieldAccess(child, requests))
}

func testCorrelatedField(correlation values.CorrelationIdentifier, name string, typ values.Type) values.Value {
	row := &values.RecordType{Fields: []values.Field{{Name: name, Ordinal: 0, FieldType: typ}}}
	qov := mustExpression(values.NewQuantifiedObjectValue(correlation, row))
	return mustResolvedField(qov, name)
}

func testField(name string, typ values.Type) values.Value {
	return testCorrelatedField(values.NamedCorrelationIdentifier("TEST_FIELD"), name, typ)
}

func testFieldAt(name string, ordinal int, typ values.Type) values.Value {
	fields := make([]values.Field, ordinal+1)
	for i := range fields {
		fields[i] = values.Field{Name: values.OrdinalFieldName(i), Ordinal: i, FieldType: values.NotNullLong}
	}
	fields[ordinal].Name = name
	fields[ordinal].FieldType = typ
	qov := mustExpression(values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier("TEST_FIELD"),
		&values.RecordType{Fields: fields},
	))
	return mustExpression(values.ResolveFieldOrdinals(qov, []int{ordinal}))
}
