package expressions

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func rowOfTypes(pairs ...any) *values.RecordType {
	fields := make([]values.Field, len(pairs)/2)
	for i := range fields {
		fields[i] = values.Field{
			Name:      pairs[2*i].(string),
			Ordinal:   i,
			FieldType: pairs[2*i+1].(values.Type),
		}
	}
	return &values.RecordType{Fields: fields}
}

func TestGetFlowedObjectTypeRejectsUnresolvedMemberInsteadOfRefining(t *testing.T) {
	t.Parallel()
	unresolved := rowOfTypes("A", values.UnknownType)
	resolved := rowOfTypes("A", values.NotNullLong)
	ref := InitialOf(&typedStubExpr{name: "unresolved", typ: unresolved})
	if !ref.Insert(&typedStubExpr{name: "resolved", typ: resolved}) {
		t.Fatal("fixture failed to retain both members")
	}
	q := NamedForEachQuantifier(values.NamedCorrelationIdentifier("Q"), ref)
	got, err := q.GetFlowedObjectType()
	if err == nil || got != nil {
		t.Fatalf("unresolved member was refined to %v with error %v; exact admission must reject it", got, err)
	}
}

func TestGetFlowedObjectTypeSupportsExactScalarRows(t *testing.T) {
	t.Parallel()
	q := NamedForEachQuantifier(
		values.NamedCorrelationIdentifier("Q"),
		InitialOf(&typedStubExpr{name: "scalar", typ: values.NotNullString}),
	)
	got, err := q.GetFlowedObjectType()
	if err != nil || !got.Equals(values.NotNullString) {
		t.Fatalf("scalar flow = (%v, %v), want %v", got, err, values.NotNullString)
	}
}
