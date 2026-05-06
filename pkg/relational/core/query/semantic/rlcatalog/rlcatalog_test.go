package rlcatalog_test

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/query/semantic"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/query/semantic/rlcatalog"
)

// Build a minimal RecordMetaData with a couple of record types —
// enough to exercise LookupTable / LookupColumn / Indexes round-trip.
func buildMetaData(t *testing.T) *recordlayer.RecordMetaData {
	t.Helper()
	b := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	b.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	b.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	b.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	md, err := b.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return md
}

func TestWrap_LookupTable(t *testing.T) {
	t.Parallel()
	md := buildMetaData(t)
	cat := rlcatalog.Wrap(md)

	tbl, ok := cat.LookupTable(semantic.ParseQualifiedName("order", false))
	if !ok {
		t.Fatal("Order should exist (case-insensitive)")
	}
	// Proto record type "Order" → SQL "ORDER" after case-folding on
	// the lookup side; but the table.Name() echoes the lookup-key
	// casing (ORDER, because NewUnquoted upper-cased).
	if tbl.Name().IsZero() {
		t.Fatal("Name should be set")
	}
}

func TestWrap_LookupTable_Missing(t *testing.T) {
	t.Parallel()
	md := buildMetaData(t)
	cat := rlcatalog.Wrap(md)

	if _, ok := cat.LookupTable(semantic.ParseQualifiedName("no_such_type", false)); ok {
		t.Fatal("nonexistent table should return false")
	}
	if cat.TableExists(semantic.ParseQualifiedName("no_such_type", false)) {
		t.Fatal("TableExists should also return false")
	}
}

func TestWrap_LookupTable_QualifiedRejected(t *testing.T) {
	t.Parallel()
	md := buildMetaData(t)
	cat := rlcatalog.Wrap(md)

	// Record Layer has no schema qualifier — qualified names don't match.
	if _, ok := cat.LookupTable(semantic.ParseQualifiedName("schema1.Order", false)); ok {
		t.Fatal("qualified name should not match (Record Layer has no schemas)")
	}
}

func TestWrap_Columns(t *testing.T) {
	t.Parallel()
	md := buildMetaData(t)
	cat := rlcatalog.Wrap(md)
	tbl, _ := cat.LookupTable(semantic.ParseQualifiedName("order", false))

	cols := tbl.Columns()
	if len(cols) == 0 {
		t.Fatal("Order should have columns")
	}
	// order_id is a known field on the Order message.
	found := false
	for _, c := range cols {
		if c.Id.EqualsIgnoreQuoting(semantic.NewUnquoted("order_id")) {
			found = true
			if c.Type != "INT" {
				t.Fatalf("order_id Type: got %q, want INT", c.Type)
			}
			break
		}
	}
	if !found {
		t.Fatal("order_id not found in Order columns")
	}
}

func TestWrap_LookupColumn(t *testing.T) {
	t.Parallel()
	md := buildMetaData(t)
	cat := rlcatalog.Wrap(md)
	tbl, _ := cat.LookupTable(semantic.ParseQualifiedName("order", false))

	col, ok := tbl.LookupColumn(semantic.NewUnquoted("ORDER_ID"))
	if !ok {
		t.Fatal("case-insensitive ORDER_ID lookup should succeed")
	}
	if col.Type != "INT" {
		t.Fatalf("Type: got %q, want INT", col.Type)
	}

	if _, ok := tbl.LookupColumn(semantic.NewUnquoted("nonexistent")); ok {
		t.Fatal("nonexistent column should miss")
	}
}

func TestNewAnalyzer(t *testing.T) {
	t.Parallel()
	md := buildMetaData(t)
	a := rlcatalog.NewAnalyzer(md, false)

	tbl, err := a.ResolveTable(semantic.ParseQualifiedName("order", false))
	if err != nil {
		t.Fatalf("resolve order: %v", err)
	}
	if tbl == nil {
		t.Fatal("Order should resolve")
	}

	// Column resolution works through the analyzer.
	col, err := a.ResolveColumn(tbl, semantic.NewUnquoted("order_id"))
	if err != nil {
		t.Fatalf("resolve order_id: %v", err)
	}
	if col.Type != "INT" {
		t.Fatalf("Type: got %q, want INT", col.Type)
	}
}

func TestWrap_AllTableNames_PreservesCasing(t *testing.T) {
	t.Parallel()
	md := buildMetaData(t)
	cat := rlcatalog.Wrap(md)

	names := cat.AllTableNames()
	if len(names) == 0 {
		t.Fatal("expected tables")
	}
	// Proto record types preserve source casing — not all-caps.
	// Find "Order" (mixed case) among the names.
	found := false
	for _, n := range names {
		if n.Name() == "Order" {
			found = true
			break
		}
	}
	if !found {
		got := make([]string, 0, len(names))
		for _, n := range names {
			got = append(got, n.String())
		}
		t.Fatalf("expected original-casing 'Order' in AllTableNames; got %v", got)
	}
}

// Indexes returns single-type and multi-type indexes on a record
// type; universal indexes intentionally stay out (different scope).
func TestWrap_Indexes(t *testing.T) {
	t.Parallel()
	b := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	b.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	b.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	b.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))

	// Add a single-type VALUE index on Order.price.
	priceIdx := recordlayer.NewIndex("order_price_idx", recordlayer.Field("price"))
	b.AddIndex("Order", priceIdx)

	md, err := b.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	cat := rlcatalog.Wrap(md)
	tbl, ok := cat.LookupTable(semantic.ParseQualifiedName("order", false))
	if !ok {
		t.Fatal("Order should exist")
	}
	idxs := tbl.Indexes()
	found := false
	for _, name := range idxs {
		if name == "order_price_idx" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected order_price_idx in Indexes; got %v", idxs)
	}
}

// protoKindToSQL mapping is a wire-compat-adjacent contract — any
// drift between proto Kind and SQL Type string would break downstream
// planner rules that dispatch on Type. TypedRecord covers every kind
// we map, plus the repeated + message cases on Order.
func TestWrap_ProtoKindToSQL_FullMapping(t *testing.T) {
	t.Parallel()
	md := buildMetaData(t)
	cat := rlcatalog.Wrap(md)
	typed, ok := cat.LookupTable(semantic.ParseQualifiedName("TypedRecord", false))
	if !ok {
		t.Fatal("TypedRecord should exist")
	}

	want := map[string]string{
		"id":           "INT",
		"val_int32":    "INT",
		"val_int64":    "INT",
		"val_sint32":   "INT",
		"val_sint64":   "INT",
		"val_sfixed32": "INT",
		"val_sfixed64": "INT",
		"val_float":    "FLOAT",
		"val_double":   "FLOAT",
		"val_bool":     "BOOL",
		"val_string":   "STRING",
		"val_bytes":    "BYTES",
		"val_enum":     "ENUM",
	}
	for colName, wantType := range want {
		col, ok := typed.LookupColumn(semantic.NewUnquoted(colName))
		if !ok {
			t.Errorf("column %q not found on TypedRecord", colName)
			continue
		}
		if col.Type != wantType {
			t.Errorf("%s.Type: got %q, want %q", colName, col.Type, wantType)
		}
	}

	// Message-typed + repeated fields on Order:
	//   flower → RECORD   (nested message)
	//   tags   → STRING   (repeated scalars still map by Kind; the
	//                       list-ness surfaces via Nullable=false)
	order, _ := cat.LookupTable(semantic.ParseQualifiedName("Order", false))
	flower, _ := order.LookupColumn(semantic.NewUnquoted("flower"))
	if flower.Type != "RECORD" {
		t.Errorf("flower.Type: got %q, want RECORD", flower.Type)
	}
	tags, _ := order.LookupColumn(semantic.NewUnquoted("tags"))
	if tags.Type != "STRING" {
		t.Errorf("tags.Type: got %q, want STRING", tags.Type)
	}
	if tags.Nullable {
		t.Error("tags is repeated → should report Nullable=false")
	}
}

func TestWrap_NilMetaData(t *testing.T) {
	t.Parallel()
	cat := rlcatalog.Wrap(nil)
	if cat.TableExists(semantic.ParseQualifiedName("anything", false)) {
		t.Fatal("nil metadata should report no tables")
	}
}

// --- Benchmarks ----------------------------------------------------

func BenchmarkWrap_LookupTable_Hit(b *testing.B) {
	bldr := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	bldr.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	bldr.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	bldr.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	md, err := bldr.Build()
	if err != nil {
		b.Fatal(err)
	}
	cat := rlcatalog.Wrap(md)
	target := semantic.ParseQualifiedName("Order", false)
	for i := 0; i < b.N; i++ {
		_, _ = cat.LookupTable(target)
	}
}

func BenchmarkWrap_LookupTable_Miss(b *testing.B) {
	bldr := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	bldr.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	bldr.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	bldr.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	md, err := bldr.Build()
	if err != nil {
		b.Fatal(err)
	}
	cat := rlcatalog.Wrap(md)
	target := semantic.ParseQualifiedName("nonexistent", false)
	for i := 0; i < b.N; i++ {
		_, _ = cat.LookupTable(target)
	}
}

func BenchmarkWrap_LookupColumn(b *testing.B) {
	bldr := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	bldr.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	bldr.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	bldr.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	md, err := bldr.Build()
	if err != nil {
		b.Fatal(err)
	}
	cat := rlcatalog.Wrap(md)
	tbl, _ := cat.LookupTable(semantic.ParseQualifiedName("Order", false))
	target := semantic.NewUnquoted("order_id")
	for i := 0; i < b.N; i++ {
		_, _ = tbl.LookupColumn(target)
	}
}
