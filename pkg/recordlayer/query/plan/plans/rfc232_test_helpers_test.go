package plans

import (
	"fmt"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func mustChecked[T any](t testing.TB, constructor func() (T, error)) T {
	t.Helper()
	result, err := constructor()
	if err != nil {
		t.Fatalf("construction failed: %v", err)
	}
	return result
}

func exactTestRecordType() values.Type {
	return values.NewRecordType("test_row", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "K", FieldType: values.NotNullLong, Ordinal: 1},
		{Name: "M", FieldType: values.NotNullLong, Ordinal: 2},
		{Name: "V", FieldType: values.NullableLong, Ordinal: 3},
		{Name: "NAME", FieldType: values.NullableString, Ordinal: 4},
	})
}

func exactEmptyRecordValue() values.Value {
	return values.NewRecordConstructorValue()
}

func mustTestQOV(t testing.TB, alias string, flowedType values.Type) values.QuantifiedObjectValue {
	t.Helper()
	result, err := values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier(alias), flowedType)
	if err != nil {
		t.Fatalf("test QOV %q: %v", alias, err)
	}
	return result
}

func testFieldIn(t testing.TB, rowType values.Type, alias, name string) values.Value {
	t.Helper()
	recordType, ok := rowType.(*values.RecordType)
	if !ok {
		t.Fatalf("test field %q root is %T, want *values.RecordType", name, rowType)
	}
	ordinal, ok := recordType.FieldIndexUnique(name)
	if !ok {
		t.Fatalf("test row %q does not declare field %q", recordType.RecordName, name)
	}
	request, err := values.FieldByNameAndOrdinal(name, ordinal)
	if err != nil {
		t.Fatalf("test field %q request: %v", name, err)
	}
	field, err := values.ResolveFieldAccess(
		mustTestQOV(t, alias, rowType),
		[]values.FieldRequest{request},
	)
	if err != nil {
		t.Fatalf("test field %q: %v", name, err)
	}
	return field
}

func testField(t testing.TB, name string, typ values.Type) values.Value {
	t.Helper()
	if typ == nil || typ.Code() == values.TypeCodeUnknown {
		t.Fatalf("test field %q requires an exact type, got %v", name, typ)
	}
	rowType := values.NewRecordType("test_"+name, false, []values.Field{
		{Name: name, FieldType: typ, Ordinal: 0},
	})
	return testFieldIn(t, rowType, "test_field_"+name, name)
}

func testFieldAt(t testing.TB, name string, ordinal int, typ values.Type) values.Value {
	t.Helper()
	if ordinal < 0 {
		t.Fatalf("test field %q has negative ordinal %d", name, ordinal)
	}
	if typ == nil || typ.Code() == values.TypeCodeUnknown {
		t.Fatalf("test field %q requires an exact type, got %v", name, typ)
	}
	fields := make([]values.Field, ordinal+1)
	for i := range fields {
		fieldName := fmt.Sprintf("_padding_%d", i)
		if i == ordinal {
			fieldName = name
		}
		fields[i] = values.Field{Name: fieldName, FieldType: typ, Ordinal: i}
	}
	rowType := values.NewRecordType(fmt.Sprintf("test_%s_at_%d", name, ordinal), false, fields)
	return testFieldIn(t, rowType, fmt.Sprintf("test_field_%s_at_%d", name, ordinal), name)
}

func testFieldName(value values.Value) string {
	field, ok := values.AsFieldValue(value)
	if !ok {
		return fmt.Sprintf("<not-field:%T>", value)
	}
	return field.DisplayName()
}
