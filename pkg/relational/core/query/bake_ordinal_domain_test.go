package query

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func bakedRef(t testing.TB, value values.Value) values.FieldValue {
	t.Helper()
	return exactTestFieldView(t, value)
}

func TestPullUpToOutputFieldResolvesAgainstExactOutputOwner(t *testing.T) {
	t.Parallel()

	sourceType := &values.RecordType{Fields: []values.Field{
		{Name: "K", Ordinal: 0, FieldType: values.NotNullLong},
	}}
	source := exactTestField(t, exactTestQOV(t, "SOURCE", sourceType), 0)
	fields := []values.RecordConstructorField{{Name: "RENAMED", Value: source}}
	outputType := &values.RecordType{Fields: []values.Field{
		{Name: "RENAMED", Ordinal: 0, FieldType: values.NotNullLong},
	}}
	output := exactTestQOV(t, "OUTPUT", outputType)

	pulled, ok := pullUpToOutputField(source, fields, output)
	if !ok {
		t.Fatal("exact source value did not pull up to its materialized output slot")
	}
	field := bakedRef(t, pulled)
	owner, ownerOK := values.AsQuantifiedObjectValue(field.ChildValue())
	if !ownerOK || owner.Correlation() != output.Correlation() {
		t.Fatalf("pulled owner = %v, want output owner %v", owner, output.Correlation())
	}
	if got := field.Path().Ordinals(); len(got) != 1 || got[0] != 0 {
		t.Fatalf("pulled path = %v, want [0]", got)
	}

	badOutput := exactTestQOV(t, "BAD_OUTPUT", &values.RecordType{Fields: []values.Field{
		{Name: "RENAMED", Ordinal: 0, FieldType: values.NotNullString},
	}})
	if _, ok := pullUpToOutputField(source, fields, badOutput); ok {
		t.Fatal("pull-up accepted an output slot with a different exact type")
	}
}
