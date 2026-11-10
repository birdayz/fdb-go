package metadata

import (
	"reflect"
	"testing"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades"
	"fdb.dev/pkg/relational/api"
	"google.golang.org/protobuf/proto"
)

// Compile-time check that RecordLayerIndex satisfies cascades.IndexDef.
var _ cascades.IndexDef = (*RecordLayerIndex)(nil)

func TestRecordLayerIndex_IndexDef(t *testing.T) {
	t.Parallel()
	idx := recordlayer.NewIndex("Order$status", recordlayer.Concat(
		recordlayer.Field("status"),
		recordlayer.Field("date"),
	))
	rli := newIndex(idx, "Order")

	if rli.IndexName() != "Order$status" {
		t.Fatalf("IndexName()=%q", rli.IndexName())
	}
	cols := rli.IndexColumnNames()
	if len(cols) != 2 || cols[0] != "status" || cols[1] != "date" {
		t.Fatalf("IndexColumnNames()=%v", cols)
	}
	types := rli.IndexRecordTypes()
	if len(types) != 1 || types[0] != "Order" {
		t.Fatalf("IndexRecordTypes()=%v", types)
	}
	if rli.IndexIsUnique() {
		t.Fatal("should not be unique")
	}
}

func TestRecordLayerIndexPartitionCoveringKeyColumns(t *testing.T) {
	t.Parallel()
	plainRoot := recordlayer.KeyWithValue(recordlayer.Concat(recordlayer.Field("ID"), recordlayer.Field("BODY")), 1)
	plain := newIndex(recordlayer.NewIndex("PLAIN_COVER", plainRoot), "PLAIN")
	if got := plain.IndexColumnNames(); !reflect.DeepEqual(got, []string{"ID", "BODY"}) {
		t.Fatalf("ordinary covering column presentation changed: %v", got)
	}
	if plain.LogicalExpression() != plainRoot || plain.Underlying().RootExpression != plainRoot {
		t.Fatal("ordinary covering expression was replaced")
	}

	base, err := NewSchemaTemplateBuilder().SetName("partition_covering_columns").AddTable("DOCS", []ColumnSpec{
		NewColumnSpec("ID", api.NewLongType(false), 1),
		NewColumnSpec("SOURCE", api.NewLongType(false), 2),
		NewColumnSpec("BODY", api.NewStringType(true), 3),
	}, []string{"ID", "SOURCE"}).Build()
	if err != nil {
		t.Fatal(err)
	}
	union := base.Underlying().GetUnionDescriptor()
	builder := recordlayer.NewRecordMetaDataBuilder().SetRecordsWithUnionName(union.ParentFile(), string(union.Name()))
	coordinate := recordlayer.PartitionExpr(recordlayer.Field("SOURCE"), 10)
	builder.GetRecordType("DOCS").SetPrimaryKey(recordlayer.Concat(recordlayer.RecordTypeKey(), coordinate, recordlayer.Field("ID"), recordlayer.Field("SOURCE")))
	tests := []struct {
		name              string
		physical, logical recordlayer.KeyExpression
		columns           []string
		ordinals          []int
	}{
		{"COVER", recordlayer.KeyWithValue(recordlayer.Concat(coordinate, recordlayer.Field("ID"), recordlayer.Field("BODY")), 2), recordlayer.KeyWithValue(recordlayer.Concat(recordlayer.Field("ID"), recordlayer.Field("BODY")), 1), []string{"ID"}, []int{2, 0, 1, 3}},
		{"P_ONLY", recordlayer.KeyWithValue(recordlayer.Concat(coordinate, recordlayer.Field("BODY")), 1), recordlayer.KeyWithValue(recordlayer.Field("BODY"), 0), nil, []int{1, 0, 2, 3}},
		{"LITERAL", recordlayer.KeyWithValue(recordlayer.Concat(coordinate, recordlayer.Literal(int64(42)), recordlayer.Field("ID"), recordlayer.Field("BODY")), 3), recordlayer.KeyWithValue(recordlayer.Concat(recordlayer.Literal(int64(42)), recordlayer.Field("ID"), recordlayer.Field("BODY")), 2), []string{"ID"}, []int{3, 0, 2, 4}},
	}
	for _, tc := range tests {
		builder.AddIndex("DOCS", recordlayer.NewIndex(tc.name, tc.physical))
	}
	md, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}
	template, err := NewRecordLayerSchemaTemplate("partition_covering_columns", md)
	if err != nil {
		t.Fatal(err)
	}
	tables, err := template.Tables()
	if err != nil {
		t.Fatal(err)
	}
	if len(tables) != 1 || len(tables[0].Indexes()) != len(tests) {
		t.Fatal("covering adapter controls are missing")
	}
	for _, tc := range tests {
		var view *RecordLayerIndex
		for _, index := range tables[0].Indexes() {
			if index.MetadataName() == tc.name {
				view = index.(*RecordLayerIndex)
				break
			}
		}
		if view == nil {
			t.Fatalf("missing index %s", tc.name)
		}
		if got := view.IndexColumnNames(); !reflect.DeepEqual(got, tc.columns) {
			t.Fatalf("%s key columns=%v want %v", tc.name, got, tc.columns)
		}
		if !proto.Equal(view.LogicalExpression().ToKeyExpression(), tc.logical.ToKeyExpression()) {
			t.Fatalf("%s full logical wrapper or included BODY changed: %v", tc.name, view.LogicalExpression().ToKeyExpression())
		}
		if view.Underlying() != md.GetIndex(tc.name) || view.Underlying().RootExpression != tc.physical {
			t.Fatalf("%s physical metadata was replaced", tc.name)
		}
		if got := view.PartitionIndexLayout().PrimaryKeyEntryOrdinals(); !reflect.DeepEqual(got, tc.ordinals) {
			t.Fatalf("%s physical entry positions=%v want %v", tc.name, got, tc.ordinals)
		}
	}
}

// TestAddVectorIndexUsingMethodValidation: the method string is case-
// sensitive everywhere downstream (buildVectorIndex treats anything that is
// not "SPFRESH" as HNSW), so unknown or mis-cased methods must fail loudly —
// AddVectorIndexUsing("SPFresh", …) silently building an HNSW index is a
// quiet misroute a schema author cannot debug.
func TestAddVectorIndexUsingMethodValidation(t *testing.T) {
	t.Parallel()
	mk := func(method string) error {
		b := NewSchemaTemplateBuilder().SetName("vt")
		b.AddTable("DOCS", []ColumnSpec{
			NewColumnSpec("ID", api.NewLongType(false), 1),
			NewColumnSpec("EMBEDDING", api.NewVectorType(64, 3, true), 2),
		}, []string{"ID"})
		b.AddVectorIndexUsing(method, "DOCS", "V", "EMBEDDING", nil, nil)
		_, err := b.Build()
		return err
	}
	for _, bad := range []string{"SPFresh", "hnsw", "IVFPQ", ""} {
		if err := mk(bad); err == nil {
			t.Errorf("method %q: want a loud rejection, got nil", bad)
		}
	}
	for _, good := range []string{"HNSW", "SPFRESH"} {
		if err := mk(good); err != nil {
			t.Errorf("method %q must build: %v", good, err)
		}
	}
}
