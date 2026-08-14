package query

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func exactTestQOV(t testing.TB, correlation string, typ values.Type) values.QuantifiedObjectValue {
	t.Helper()
	qov, err := values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier(correlation), typ)
	if err != nil {
		t.Fatalf("exact test QOV %q: %v", correlation, err)
	}
	return qov
}

func exactTestField(t testing.TB, owner values.Value, ordinals ...int) values.Value {
	t.Helper()
	field, err := values.ResolveFieldOrdinals(owner, ordinals)
	if err != nil {
		t.Fatalf("exact test field %v: %v", ordinals, err)
	}
	return field
}

func exactTestFieldView(t testing.TB, value values.Value) values.FieldValue {
	t.Helper()
	field, ok := values.AsFieldValue(value)
	if !ok {
		t.Fatalf("value %T is not an exact field access", value)
	}
	return field
}

func exactTestNamedField(t testing.TB, correlation, name string, typ values.Type) values.Value {
	t.Helper()
	rowType := &values.RecordType{Fields: []values.Field{{Name: name, Ordinal: 0, FieldType: typ}}}
	return exactTestField(t, exactTestQOV(t, correlation, rowType), 0)
}
