package rlcatalog_test

import (
	"testing"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/core/query/semantic"
	"fdb.dev/pkg/relational/core/query/semantic/rlcatalog"
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
	// A column presents the DESCRIPTOR's own spelling — Java's
	// `ProtoUtils.toUserIdentifier(fieldDescriptor.getName())` with no fold
	// (Type.java:2875). `order_id` is declared lower-case in the demo proto,
	// so that is the name the catalog states.
	found := false
	for _, c := range cols {
		if c.Id.Name() == "order_id" {
			found = true
			if c.Type != "BIGINT" {
				t.Fatalf("order_id Type: got %q, want BIGINT", c.Type)
			}
			break
		}
	}
	if !found {
		t.Fatalf("order_id not found in Order columns: %v", columnNames(cols))
	}
	// And it is NOT also presented folded. Two spellings of one column is the
	// state this catalog used to be in, and it is what let a resolver agree
	// with itself while disagreeing with the row layout.
	for _, c := range cols {
		if c.Id.Name() == "ORDER_ID" {
			t.Fatalf("Order presents a FOLDED ORDER_ID beside order_id: %v", columnNames(cols))
		}
	}
}

func columnNames(cols []semantic.Column) []string {
	out := make([]string, 0, len(cols))
	for _, c := range cols {
		out = append(out, c.Id.Name())
	}
	return out
}

// TestWrap_LookupColumnIsExact pins the table's own lookup as EXACT — Java's
// rule, where the only normalization is at the parse boundary and every
// catalog comparison downstream is `.equals`.
//
// The case-insensitive step is deliberately NOT here. It belongs to the scope,
// which runs it across every source at a level at once; a table that relaxed on
// its own would let one source's loose match compete with another source's
// exact match and turn a reference with one right answer into 42702.
func TestWrap_LookupColumnIsExact(t *testing.T) {
	t.Parallel()
	md := buildMetaData(t)
	cat := rlcatalog.Wrap(md)
	tbl, _ := cat.LookupTable(semantic.ParseQualifiedName("order", false))

	col, ok := tbl.LookupColumn(semantic.FromNormalized("order_id"))
	if !ok {
		t.Fatal("exact order_id lookup should succeed")
	}
	if col.Type != "BIGINT" {
		t.Fatalf("Type: got %q, want BIGINT", col.Type)
	}

	// NewUnquoted folds to ORDER_ID, which no field declares.
	if _, ok := tbl.LookupColumn(semantic.NewUnquoted("ORDER_ID")); ok {
		t.Fatal("folded ORDER_ID must NOT match the exact table lookup")
	}
	if _, ok := tbl.LookupColumn(semantic.NewUnquoted("nonexistent")); ok {
		t.Fatal("nonexistent column should miss")
	}
}

// TestWrap_LookupColumnRelaxedReachesRawProtoNames pins the read-side
// extension: an unquoted SQL reference arrives folded UPPER, and a hand-written
// .proto's field names never went through DDL normalization, so
// `SELECT order_id` has to reach the field literally named `order_id`.
//
// This exists because Go does not plumb Java's CASE_SENSITIVE_IDENTIFIERS
// option (Options.java:211) and wrapping a user's own descriptor as a SQL
// catalog is a first-class entry point here, where in Java it is a corner.
func TestWrap_LookupColumnRelaxedReachesRawProtoNames(t *testing.T) {
	t.Parallel()
	md := buildMetaData(t)
	cat := rlcatalog.Wrap(md)
	tbl, _ := cat.LookupTable(semantic.ParseQualifiedName("order", false))

	col, ok := semantic.LookupColumnRelaxed(tbl, semantic.NewUnquoted("ORDER_ID"))
	if !ok {
		t.Fatal("relaxed ORDER_ID lookup should reach order_id")
	}
	if col.Id.Name() != "order_id" {
		t.Fatalf("relaxed lookup returned %q, want the declared order_id", col.Id.Name())
	}
	if _, ok := semantic.LookupColumnRelaxed(tbl, semantic.NewUnquoted("nonexistent")); ok {
		t.Fatal("nonexistent column should miss even relaxed")
	}
}

// TestWrap_NestedStructFieldKeepsItsDescriptorSpelling is the nested half of
// the same rule, and it is a separate test because nothing in the corpus
// resolves a nested field over raw-proto metadata: DDL descriptors are already
// upper, so every existing nested test passes either way.
func TestWrap_NestedStructFieldKeepsItsDescriptorSpelling(t *testing.T) {
	t.Parallel()
	md := buildMetaData(t)
	cat := rlcatalog.Wrap(md)
	tbl, _ := cat.LookupTable(semantic.ParseQualifiedName("order", false))

	flower, ok := semantic.LookupColumnRelaxed(tbl, semantic.NewUnquoted("flower"))
	if !ok {
		t.Fatal("flower column not found")
	}
	if len(flower.StructFields) == 0 {
		t.Fatal("flower must carry its nested fields")
	}
	if _, _, ok := flower.LookupStructField(semantic.FromNormalized("type")); !ok {
		t.Fatalf("exact flower.type must resolve; fields = %v", columnNames(flower.StructFields))
	}
	if _, _, ok := flower.LookupStructField(semantic.NewUnquoted("TYPE")); ok {
		t.Fatal("folded flower.TYPE must NOT match the exact struct lookup")
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
	if col.Type != "BIGINT" {
		t.Fatalf("Type: got %q, want BIGINT", col.Type)
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

	// Widths are faithful to the proto kind: 32-bit kinds are INTEGER,
	// 64-bit kinds are BIGINT, float is FLOAT and double is DOUBLE. The
	// old all-integers→"INT" / float+double→"FLOAT" conflation was
	// harmless only while the expression layer erased widths anyway.
	want := map[string]string{
		"id":           "BIGINT",
		"val_int32":    "INTEGER",
		"val_int64":    "BIGINT",
		"val_sint32":   "INTEGER",
		"val_sint64":   "BIGINT",
		"val_sfixed32": "INTEGER",
		"val_sfixed64": "BIGINT",
		"val_float":    "FLOAT",
		"val_double":   "DOUBLE",
		"val_bool":     "BOOL",
		"val_string":   "STRING",
		"val_bytes":    "BYTES",
		"val_enum":     "ENUM",
	}
	for colName, wantType := range want {
		col, ok := typed.LookupColumn(semantic.FromNormalized(colName))
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
	flower, _ := order.LookupColumn(semantic.FromNormalized("flower"))
	if flower.Type != "RECORD" {
		t.Errorf("flower.Type: got %q, want RECORD", flower.Type)
	}
	flowerDescriptor := gen.File_record_layer_demo_proto.Messages().ByName("Order").Fields().ByName("flower")
	wantStructTypeName := string(flowerDescriptor.Message().FullName())
	if flower.StructTypeName != wantStructTypeName {
		t.Errorf("flower.StructTypeName: got %q, want descriptor identity %q", flower.StructTypeName, wantStructTypeName)
	}
	if flower.StructTypeName == flower.Type {
		t.Error("flower lost its nominal descriptor identity and retained only the coarse RECORD kind")
	}
	tags, _ := order.LookupColumn(semantic.FromNormalized("tags"))
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
