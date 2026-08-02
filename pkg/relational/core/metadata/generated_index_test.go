package metadata

import (
	"testing"

	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
)

// TestAddGeneratedIndex_CarriedVerbatim pins RFC-202 S1/D8: an explicit index
// description (root expression, type, options, predicate, unique) survives
// Build() verbatim — no name-based re-derivation of the key expression.
func TestAddGeneratedIndex_CarriedVerbatim(t *testing.T) {
	t.Parallel()

	pred := &gen.Predicate{
		ConstantPredicate: &gen.ConstantPredicate{
			Value: gen.ConstantPredicate_TRUE.Enum(),
		},
	}
	root := recordlayer.KeyWithValue(
		recordlayer.Concat(recordlayer.Field("A"), recordlayer.Field("B")), 1)

	b := NewSchemaTemplateBuilder().SetName("tpl")
	b.AddTable("T", []ColumnSpec{
		NewColumnSpec("A", api.NewLongType(true), 1),
		NewColumnSpec("B", api.NewLongType(true), 2),
	}, []string{"A"})
	b.AddGeneratedIndex("T", "GIDX", root, recordlayer.IndexTypeValue, true,
		map[string]string{"someOption": "7"}, pred)

	tmpl, err := b.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	md := tmpl.Underlying()
	var idx *recordlayer.Index
	for _, i := range md.GetAllIndexes() {
		if i.Name == "GIDX" {
			idx = i
		}
	}
	if idx == nil {
		t.Fatal("GIDX not found in built metadata")
	}
	wantRoot := root.ToKeyExpression()
	gotRoot := idx.RootExpression.ToKeyExpression()
	if !proto.Equal(wantRoot, gotRoot) {
		t.Errorf("root expression mutated by Build():\nwant %v\ngot  %v", wantRoot, gotRoot)
	}
	if idx.Type != recordlayer.IndexTypeValue {
		t.Errorf("type = %q, want value", idx.Type)
	}
	if !idx.IsUnique() {
		t.Error("unique dropped by Build()")
	}
	if idx.Options["someOption"] != "7" {
		t.Errorf("options dropped: %v", idx.Options)
	}
	if !proto.Equal(idx.GetPredicateProto(), pred) {
		t.Errorf("predicate dropped or mutated: %v", idx.GetPredicateProto())
	}
}

// TestAddGeneratedIndex_UnknownTable pins the deferred-error contract shared
// with every other Add* method.
func TestAddGeneratedIndex_UnknownTable(t *testing.T) {
	t.Parallel()
	b := NewSchemaTemplateBuilder().SetName("tpl")
	b.AddTable("T", []ColumnSpec{NewColumnSpec("A", api.NewLongType(true), 1)}, []string{"A"})
	b.AddGeneratedIndex("NOPE", "GIDX", recordlayer.Field("A"), "", false, nil, nil)
	if _, err := b.Build(); err == nil {
		t.Fatal("Build succeeded with an index on an unknown table")
	}
}
