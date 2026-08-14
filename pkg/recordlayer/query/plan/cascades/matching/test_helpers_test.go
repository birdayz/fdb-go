package matching

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func mustMatchingField(t testing.TB, name string, typ values.Type) values.FieldValue {
	t.Helper()
	rootType := &values.RecordType{Fields: []values.Field{{
		Name: name, Ordinal: 0, FieldType: typ,
	}}}
	root, err := values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier("matching_"+name), rootType)
	if err != nil {
		t.Fatalf("construct matching field root: %v", err)
	}
	request, err := values.FieldByNameAndOrdinal(name, 0)
	if err != nil {
		t.Fatalf("construct matching field request: %v", err)
	}
	resolved, err := values.ResolveFieldAccess(root, []values.FieldRequest{request})
	if err != nil {
		t.Fatalf("resolve matching field: %v", err)
	}
	field, ok := values.AsFieldValue(resolved)
	if !ok {
		t.Fatalf("resolved matching field is %T, want admitted FieldValue", resolved)
	}
	return field
}
