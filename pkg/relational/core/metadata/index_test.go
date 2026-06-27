package metadata

import (
	"testing"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades"
	"fdb.dev/pkg/relational/api"
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

// TestAddVectorIndexUsingMethodValidation: the method string is case-
// sensitive everywhere downstream (buildVectorIndex treats anything that is
// not "SPFRESH" as HNSW), so unknown or mis-cased methods must fail loudly —
// AddVectorIndexUsing("SPFresh", …) silently building an HNSW index is a
// quiet misroute a schema author cannot debug (Graefe merge-HEAD F2).
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
