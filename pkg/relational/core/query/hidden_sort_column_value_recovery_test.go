package query

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/logical"
)

const unfindableHiddenName = "\x00 no name scan can find this \x00"

func TestHiddenSortColumnIsRecoveredByExactValueNotByName(t *testing.T) {
	t.Parallel()

	sourceType := &values.RecordType{Fields: []values.Field{
		{Name: "ID", Ordinal: 0, FieldType: values.NotNullLong},
		{Name: "COL1", Ordinal: 1, FieldType: values.NotNullLong},
	}}
	source := exactTestQOV(t, "T1", sourceType)
	id := exactTestField(t, source, 0)
	col1 := exactTestField(t, source, 1)
	key := logical.SortKey{Expr: "COL1", Value: col1}
	fields := []values.RecordConstructorField{
		{Name: "ID", Value: id},
		{Name: "H", Value: values.LiteralValue(true)},
		{Name: unfindableHiddenName, Value: col1},
	}
	outputType := &values.RecordType{Fields: []values.Field{
		{Name: "ID", Ordinal: 0, FieldType: values.NotNullLong},
		{Name: "H", Ordinal: 1, FieldType: values.NotNullBoolean},
		{Name: unfindableHiddenName, Ordinal: 2, FieldType: values.NotNullLong},
	}}
	output := exactTestQOV(t, "FOLDED", outputType)

	got := pullUpSortKeyValue(key, key.Value, fields, sortSource{isJoin: false}, output)
	fv := exactTestFieldView(t, got)
	if ordinals := fv.Path().Ordinals(); len(ordinals) != 1 || ordinals[0] != 2 {
		t.Fatalf("pulled-up path = %v, want hidden output slot [2]", ordinals)
	}
	if fv.DisplayName() != unfindableHiddenName {
		t.Fatalf("pulled-up display name = %q, want %q", fv.DisplayName(), unfindableHiddenName)
	}
	owner, ok := values.AsQuantifiedObjectValue(fv.ChildValue())
	if !ok || owner.Correlation() != output.Correlation() || !owner.FlowedType().Equals(outputType) {
		t.Fatalf("pulled-up owner = %v, want exact folded-output QOV", owner)
	}
}

func TestHiddenSortColumnNameIsNotTheRecoveryAuthority(t *testing.T) {
	t.Parallel()

	sourceType := &values.RecordType{Fields: []values.Field{
		{Name: "OTHER", Ordinal: 0, FieldType: values.NotNullLong},
		{Name: "COL1", Ordinal: 1, FieldType: values.NotNullLong},
	}}
	source := exactTestQOV(t, "T1", sourceType)
	other := exactTestField(t, source, 0)
	col1 := exactTestField(t, source, 1)
	key := logical.SortKey{Expr: "COL1", Value: col1}
	fields := []values.RecordConstructorField{
		{Name: "COL1", Value: other},
		{Name: unfindableHiddenName, Value: col1},
	}
	outputType := &values.RecordType{Fields: []values.Field{
		{Name: "COL1", Ordinal: 0, FieldType: values.NotNullLong},
		{Name: unfindableHiddenName, Ordinal: 1, FieldType: values.NotNullLong},
	}}
	output := exactTestQOV(t, "FOLDED", outputType)

	got := pullUpSortKeyValue(key, key.Value, fields, sortSource{isJoin: false}, output)
	fv := exactTestFieldView(t, got)
	if ordinals := fv.Path().Ordinals(); len(ordinals) != 1 || ordinals[0] != 1 {
		t.Fatalf("sort key resolved to %v, want hidden value-matching slot [1]; slot 0 is the same-named decoy", ordinals)
	}
}
