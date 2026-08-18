package logical

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

var _ LogicalOperator = (*LogicalInlineValues)(nil)

func TestNewInlineValuesFreezesExactRowType(t *testing.T) {
	t.Parallel()

	one := &values.ConstantValue{Value: int64(1), Typ: values.NotNullLong}
	oneHundredOne := &values.ConstantValue{Value: int64(101), Typ: values.NotNullLong}
	row := values.NewRawRecordConstructorValue(
		values.RecordConstructorField{Name: "ID", Value: one},
		values.RecordConstructorField{
			Name: "ARR",
			Value: values.NewArrayConstructorValue(values.NotNullLong, []values.Value{
				oneHundredOne,
			}),
		},
	)
	// The row type is spelled out rather than taken from row.Type(), because the
	// mutation below is the point of this test and it must land in a graph THIS
	// function owns. A graph from an accessor carries the callee's provenance,
	// and under RFC-234 several accessors hand back the graph cached on an
	// interned handle — mutating one of those corrupts every value flowing the
	// shape, including in tests running in parallel. Asserted equal to row.Type()
	// first, so spelling it out cannot drift from what the constructor produces.
	rowType := &values.RecordType{Fields: []values.Field{
		{Name: "ID", Ordinal: 0, FieldType: values.NotNullLong},
		{Name: "ARR", Ordinal: 1, FieldType: &values.ArrayType{ElementType: values.NotNullLong}},
	}}
	if !rowType.Equals(row.Type()) {
		t.Fatalf("the spelled-out row type %v has drifted from the constructor's %v",
			rowType, row.Type())
	}
	collection := values.NewArrayConstructorValue(rowType, []values.Value{row})

	source, err := NewInlineValues("V", collection)
	if err != nil {
		t.Fatalf("NewInlineValues: %v", err)
	}
	if source.CollectionValue() != collection {
		t.Fatal("collection identity was not preserved")
	}
	if len(source.Children()) != 0 {
		t.Fatalf("Children = %d, want 0", len(source.Children()))
	}
	if got := source.Explain("  "); !strings.HasPrefix(got, "  InlineValues(") || !strings.HasSuffix(got, " AS V)") {
		t.Fatalf("Explain = %q, want indented InlineValues(... AS V)", got)
	}

	result, ok := source.ResultType().(*values.RecordType)
	if !ok {
		t.Fatalf("ResultType = %T, want *values.RecordType", source.ResultType())
	}
	if !result.Equals(rowType) {
		t.Fatalf("ResultType = %v, want %v", result, rowType)
	}
	if len(result.Fields) != 2 || result.Fields[0].Name != "ID" || result.Fields[1].Name != "ARR" {
		t.Fatalf("ResultType fields = %#v, want [ID, ARR]", result.Fields)
	}

	// The public row type is an immutable snapshot, not a borrowed pointer into
	// the collection's mutable ordinary Type graph.
	rowType.Fields[0].Name = "MUTATED"
	again := source.ResultType().(*values.RecordType)
	if again.Fields[0].Name != "ID" {
		t.Fatalf("frozen ResultType changed through source mutation: %#v", again.Fields)
	}
}

func TestNewInlineValuesRejectsInexactOrNonRecordCollections(t *testing.T) {
	t.Parallel()

	exactRow := &values.RecordType{Fields: []values.Field{
		{Name: "ID", Ordinal: 0, FieldType: values.NotNullLong},
	}}
	tests := []struct {
		name       string
		alias      string
		collection values.Value
	}{
		{name: "empty alias", collection: values.NewArrayConstructorValue(exactRow, nil)},
		{name: "nil collection", alias: "V"},
		{name: "not array", alias: "V", collection: &values.ConstantValue{Value: int64(1), Typ: values.NotNullLong}},
		{name: "scalar elements", alias: "V", collection: values.NewArrayConstructorValue(values.NotNullLong, nil)},
		{name: "unknown row field", alias: "V", collection: values.NewArrayConstructorValue(
			&values.RecordType{Fields: []values.Field{{Name: "ID", Ordinal: 0, FieldType: values.UnknownType}}}, nil)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if source, err := NewInlineValues(test.alias, test.collection); err == nil {
				t.Fatalf("NewInlineValues returned (%#v, nil), want rejection", source)
			}
		})
	}
}
