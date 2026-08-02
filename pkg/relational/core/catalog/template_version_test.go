package catalog

import (
	"testing"

	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/metadata"
)

// TestSerializeTemplateStoresTheBuiltVersion pins the stored metadata
// version against Java's arithmetic: the builder is seeded with the SQL
// template version (Java RecordMetadataSerializer.visit:112, run first by
// RecordLayerSchemaTemplate.accept:378-380) and every index bumps it
// (RecordMetaDataBuilder.addIndexCommon:1093-1097). Java therefore stores
// templateVersion + #indexes, with each index's lastModifiedVersion inside
// that range — never a stored version BELOW an index that claims to have
// been modified after it.
//
// This is the dimension that index-free coverage cannot see: with no
// indexes the built version and the template version coincide, so an
// override of one by the other is invisible. The template below carries
// two, and the assertion is on the relationship, not only the number — a
// stored version that does not dominate every index's lastModifiedVersion
// is metadata a Java reader silently repairs upward, and which
// GetIndexesToBuildSince reads as "rebuild everything".
func TestSerializeTemplateStoresTheBuiltVersion(t *testing.T) {
	t.Parallel()

	// The metadata builder rather than the DDL front end: this package is
	// imported BY the SQL front end, so the front end cannot be imported
	// here. The shape is the same one the value_indexes byte-golden drives
	// through Java's DDL — two tables, two indexes.
	b := metadata.NewSchemaTemplateBuilder().SetName("versioned-tmpl")
	b.AddTable("T1", []metadata.ColumnSpec{
		metadata.NewColumnSpec("ID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("NAME", api.NewStringType(true), 2),
	}, []string{"ID"})
	b.AddTable("T2", []metadata.ColumnSpec{
		metadata.NewColumnSpec("ID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("TAG", api.NewStringType(true), 2),
	}, []string{"ID"})
	b.AddIndex("T1", "IDX_NAME", []string{"NAME"}, false)
	b.AddIndex("T2", "IDX_TAG", []string{"TAG"}, false)
	rl, err := b.Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if got := rl.Version(); got != 1 {
		t.Fatalf("SQL template version: got %d, want 1", got)
	}

	payload, err := serializeTemplate(rl)
	if err != nil {
		t.Fatalf("serialize: %v", err)
	}
	p := &gen.MetaData{}
	if err := proto.Unmarshal(payload, p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// templateVersion(1) + 2 indexes.
	if got := p.GetVersion(); got != 3 {
		t.Errorf("stored metadata version: got %d, want 3 (template version 1 + 2 indexes)", got)
	}
	idx := rl.Underlying().GetAllIndexes()
	if len(idx) != 2 {
		t.Fatalf("indexes: got %d, want 2", len(idx))
	}
	for name, i := range idx {
		if i.LastModifiedVersion > int(p.GetVersion()) {
			t.Errorf("index %s lastModifiedVersion %d exceeds the stored metadata version %d — "+
				"a Java reader repairs that upward and GetIndexesToBuildSince treats every index as pending",
				name, i.LastModifiedVersion, p.GetVersion())
		}
		if i.LastModifiedVersion <= 1 {
			t.Errorf("index %s lastModifiedVersion %d did not advance past the template version — "+
				"Java bumps it once per index (addIndexCommon:1093-1097)", name, i.LastModifiedVersion)
		}
	}
}
