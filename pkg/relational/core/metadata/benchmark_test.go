package metadata

import (
	"testing"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
)

// benchmarkMetaData builds a reusable *recordlayer.RecordMetaData for
// benchmarks. Keeps the schema modest (3 message types, 1 index) so
// the bridge cost dominates rather than the proto descriptor tree.
func benchmarkMetaData(b *testing.B) *recordlayer.RecordMetaData {
	b.Helper()
	bu := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	bu.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	bu.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	bu.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	bu.AddIndex("Order", recordlayer.NewIndex("order_by_price", recordlayer.Field("price")))
	md, err := bu.Build()
	if err != nil {
		b.Fatalf("Build: %v", err)
	}
	return md
}

// BenchmarkNewSchemaTemplate measures the cost of materialising a
// template from an already-built RecordMetaData. Dominant work is
// proto-descriptor traversal for every message type.
func BenchmarkNewSchemaTemplate(b *testing.B) {
	md := benchmarkMetaData(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := NewRecordLayerSchemaTemplate("bench", md); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkFindTable measures the post-construction lookup hot path —
// this is what the semantic analyzer will hammer on every SQL parse.
func BenchmarkFindTable(b *testing.B) {
	tmpl, err := NewRecordLayerSchemaTemplate("bench", benchmarkMetaData(b))
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		t, err := tmpl.FindTable("Order")
		if err != nil || t == nil {
			b.Fatalf("FindTable: t=%v err=%v", t, err)
		}
	}
}

// BenchmarkTableColumns measures Columns() access cost. Should be
// O(1) — the column slice is materialised at construction.
func BenchmarkTableColumns(b *testing.B) {
	tmpl, _ := NewRecordLayerSchemaTemplate("bench", benchmarkMetaData(b))
	order, _ := tmpl.FindTable("Order")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = order.Columns()
	}
}

// BenchmarkTableIndexMapping measures the per-template index-map
// computation. Currently re-computes on every call; if that shows up
// on profiles we can memoise.
func BenchmarkTableIndexMapping(b *testing.B) {
	tmpl, _ := NewRecordLayerSchemaTemplate("bench", benchmarkMetaData(b))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := tmpl.TableIndexMapping(); err != nil {
			b.Fatal(err)
		}
	}
}
