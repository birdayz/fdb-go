package query

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/logical"
)

// rawBoundUnnest builds the semantic collection carried by hand-written logical
// IR. The layout and ordinal path are fixture inputs; Segments is diagnostic
// syntax and is deliberately not consulted to resolve the collection.
func rawBoundUnnest(
	t testing.TB,
	segments []string,
	alias, atAlias, ownerCorrelation string,
	ownerLayout *values.RecordType,
	ordinals ...int,
) (*logical.LogicalUnnest, values.Type) {
	t.Helper()
	collection := rawCorrelatedValue(t, ownerCorrelation, ownerLayout, ordinals...)
	array, ok := collection.Type().(*values.ArrayType)
	if !ok || array.ElementType == nil {
		t.Fatalf("raw unnest collection path %v has type %v, want exact ARRAY", ordinals, collection.Type())
	}
	return &logical.LogicalUnnest{
		Segments:             append([]string(nil), segments...),
		Alias:                alias,
		AtAlias:              atAlias,
		CorrelatedCollection: collection,
	}, array.ElementType
}

func rawCorrelatedValue(t testing.TB, ownerCorrelation string, ownerLayout *values.RecordType, ordinals ...int) values.Value {
	t.Helper()
	owner, err := values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier(ownerCorrelation), ownerLayout)
	if err != nil {
		t.Fatalf("raw correlated owner: %v", err)
	}
	value, err := values.ResolveFieldOrdinals(owner, ordinals)
	if err != nil {
		t.Fatalf("raw correlated path %v: %v", ordinals, err)
	}
	return value
}

// rawBoundPriorElementUnnest binds a chained collection to the preceding AS
// correlation. WITH ORDINALITY makes that owner a two-field [element, ordinal]
// row, so semantic descent starts at ordinal 0. Without AT the QOV flows the
// whole element and memberOrdinals starts directly at that element.
func rawBoundPriorElementUnnest(
	t testing.TB,
	priorAlias, priorAtAlias string,
	elementLayout *values.RecordType,
	segments []string,
	alias, atAlias string,
	memberOrdinals ...int,
) (*logical.LogicalUnnest, values.Type) {
	t.Helper()
	ownerLayout := elementLayout
	path := append([]int(nil), memberOrdinals...)
	if priorAtAlias != "" {
		ownerLayout = &values.RecordType{Fields: []values.Field{
			{Name: priorAlias, Ordinal: 0, FieldType: elementLayout},
			{Name: priorAtAlias, Ordinal: 1, FieldType: values.NotNullInt},
		}}
		path = append([]int{0}, path...)
	}
	return rawBoundUnnest(t, segments, alias, atAlias, priorAlias, ownerLayout, path...)
}

// rawProtoRowLayout authors a fixture input layout directly from the fixture
// protobuf descriptor. It does not consult translator-derived output types.
func rawProtoRowLayout(t testing.TB, md *recordlayer.RecordMetaData, table string) *values.RecordType {
	t.Helper()
	rt := md.GetRecordType(table)
	if rt == nil || rt.Descriptor == nil {
		t.Fatalf("fixture table %q has no protobuf descriptor", table)
	}
	fields := rt.Descriptor.Fields()
	out := &values.RecordType{Fields: make([]values.Field, fields.Len())}
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		out.Fields[i] = values.Field{
			Name:      values.FieldNameForProtoField(fd),
			Ordinal:   i,
			FieldType: values.FieldTypeForProtoField(fd),
		}
	}
	return out
}

func rawProtoPath(t testing.TB, md *recordlayer.RecordMetaData, table string, names ...string) (*values.RecordType, []int) {
	t.Helper()
	layout := rawProtoRowLayout(t, md, table)
	current := values.Type(layout)
	ordinals := make([]int, len(names))
	for i, name := range names {
		record, ok := current.(*values.RecordType)
		if !ok {
			t.Fatalf("fixture path %v reaches non-record %v before %q", names, current, name)
		}
		ordinal := -1
		for j, field := range record.Fields {
			if strings.EqualFold(field.Name, name) {
				if ordinal >= 0 {
					t.Fatalf("fixture path %v is ambiguous at %q", names, name)
				}
				ordinal = j
			}
		}
		if ordinal < 0 {
			t.Fatalf("fixture path %v has no field %q", names, name)
		}
		ordinals[i] = ordinal
		current = record.Fields[ordinal].FieldType
	}
	return layout, ordinals
}

func rawBoundProtoUnnest(
	t testing.TB,
	md *recordlayer.RecordMetaData,
	table, ownerCorrelation string,
	segments []string,
	alias, atAlias string,
	fieldPath ...string,
) (*logical.LogicalUnnest, values.Type) {
	t.Helper()
	layout, ordinals := rawProtoPath(t, md, table, fieldPath...)
	return rawBoundUnnest(t, segments, alias, atAlias, ownerCorrelation, layout, ordinals...)
}

// rawNullSuppliedLayout authors the semantic owner row after an outer join
// pads that source. Only top-level columns become nullable, not nested elements.
func rawNullSuppliedLayout(stored *values.RecordType) *values.RecordType {
	row := &values.RecordType{Fields: make([]values.Field, len(stored.Fields))}
	for i, field := range stored.Fields {
		row.Fields[i] = values.Field{Name: field.Name, Ordinal: i, FieldType: values.WithNullability(field.FieldType, true)}
	}
	return row
}
