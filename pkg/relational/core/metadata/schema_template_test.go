package metadata

import (
	"errors"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
)

// buildTestTemplate returns a RecordLayerSchemaTemplate built from the
// conformance demo proto with minimal configuration. Registers Order /
// Customer / TypedRecord as tables with int64 primary keys.
func buildTestTemplate(t *testing.T) *RecordLayerSchemaTemplate {
	t.Helper()
	b := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	b.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	b.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	b.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	md, err := b.Build()
	if err != nil {
		t.Fatalf("build metadata: %v", err)
	}
	tmpl, err := NewRecordLayerSchemaTemplate("demo", md)
	if err != nil {
		t.Fatalf("NewRecordLayerSchemaTemplate: %v", err)
	}
	return tmpl
}

func TestSchemaTemplate_BasicShape(t *testing.T) {
	t.Parallel()
	tmpl := buildTestTemplate(t)

	if tmpl.MetadataName() != "demo" {
		t.Errorf("MetadataName = %q, want %q", tmpl.MetadataName(), "demo")
	}
	// Version starts at 0 after Build + three SetPrimaryKey calls on the
	// builder. It's not 0 exactly — what matters is that our bridge
	// returns whatever the metadata does (we're a pass-through).
	if got, want := tmpl.Version(), tmpl.Underlying().Version(); got != want {
		t.Errorf("Version = %d, want underlying %d", got, want)
	}
	if tmpl.EnableLongRows() != tmpl.Underlying().IsSplitLongRecords() {
		t.Error("EnableLongRows did not mirror IsSplitLongRecords")
	}
	if tmpl.StoreRowVersions() != tmpl.Underlying().IsStoreRecordVersions() {
		t.Error("StoreRowVersions did not mirror IsStoreRecordVersions")
	}
}

func TestSchemaTemplate_VersionIndependentFromMetadata(t *testing.T) {
	t.Parallel()
	// Java's RecordLayerSchemaTemplate.Version() is catalog-level and
	// decoupled from RecordMetaData.Version(). The default
	// constructor mirrors fromRecordMetadata(md, name, md.getVersion())
	// but the explicit-version constructor lets the catalog pin a
	// different number.
	b := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	b.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	b.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	b.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	md, err := b.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// Default constructor: version == md.Version().
	def, err := NewRecordLayerSchemaTemplate("demo", md)
	if err != nil {
		t.Fatalf("NewRecordLayerSchemaTemplate: %v", err)
	}
	if def.Version() != md.Version() {
		t.Errorf("default Version = %d, want md.Version = %d", def.Version(), md.Version())
	}

	// Explicit-version constructor: catalog-level version can diverge.
	catalogVersion := md.Version() + 42
	pinned, err := NewRecordLayerSchemaTemplateWithVersion("demo", md, catalogVersion)
	if err != nil {
		t.Fatalf("NewRecordLayerSchemaTemplateWithVersion: %v", err)
	}
	if pinned.Version() != catalogVersion {
		t.Errorf("pinned Version = %d, want %d", pinned.Version(), catalogVersion)
	}
	if pinned.Underlying().Version() == catalogVersion {
		t.Error("pinned template must not mutate the underlying RecordMetaData version")
	}
}

func TestSchemaTemplate_NilMetaDataReturnsError(t *testing.T) {
	t.Parallel()
	// Passing a nil *RecordMetaData must fail cleanly, not panic.
	// Callers are boundary code (DSN parsing, RPC handlers) and a
	// panic here would crash the whole driver.
	for _, name := range []string{"default ctor", "explicit-version ctor"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var (
				tmpl *RecordLayerSchemaTemplate
				err  error
			)
			if name == "default ctor" {
				tmpl, err = NewRecordLayerSchemaTemplate("nil-md", nil)
			} else {
				tmpl, err = NewRecordLayerSchemaTemplateWithVersion("nil-md", nil, 0)
			}
			if err == nil {
				t.Fatal("expected error when md == nil, got nil")
			}
			if tmpl != nil {
				t.Errorf("got non-nil template alongside error: %v", tmpl)
			}
			var apiErr *api.Error
			if !errors.As(err, &apiErr) {
				t.Fatalf("err is not *api.Error: %v", err)
			}
			if apiErr.Code != api.ErrCodeInvalidSchemaTemplate {
				t.Errorf("Code = %q, want %q", apiErr.Code, api.ErrCodeInvalidSchemaTemplate)
			}
		})
	}
}

func TestSchemaTemplate_IntermingleTables(t *testing.T) {
	t.Parallel()
	// Mirror Java: intermingleTables is the negation of
	// primaryKeyHasRecordTypePrefix. In buildTestTemplate every record
	// type has a PK like Field("order_id") — no RecordTypeKey prefix,
	// so rows are intermingled.
	tmpl := buildTestTemplate(t)
	if !tmpl.IntermingleTables() {
		t.Error("IntermingleTables = false, want true (PKs have no RecordTypeKey prefix)")
	}
}

func TestSchemaTemplate_Tables(t *testing.T) {
	t.Parallel()
	tmpl := buildTestTemplate(t)

	tables, err := tmpl.Tables()
	if err != nil {
		t.Fatalf("Tables: %v", err)
	}
	// demo proto has Order, Customer, TypedRecord.
	names := tableNames(tables)
	expectContains(t, names, "Order", "Customer", "TypedRecord")

	// Order should come before TypedRecord (sorted order).
	for i := 1; i < len(names); i++ {
		if names[i-1] > names[i] {
			t.Errorf("Tables() not sorted: %v", names)
		}
	}
}

func TestSchemaTemplate_FindTable(t *testing.T) {
	t.Parallel()
	tmpl := buildTestTemplate(t)

	found, err := tmpl.FindTable("Order")
	if err != nil {
		t.Fatalf("FindTable(Order): %v", err)
	}
	if found == nil {
		t.Fatal("FindTable(Order) returned nil")
	}
	if found.MetadataName() != "Order" {
		t.Errorf("got name %q", found.MetadataName())
	}

	missing, err := tmpl.FindTable("NotATable")
	if err != nil {
		t.Fatalf("FindTable(missing) returned error: %v", err)
	}
	if missing != nil {
		t.Errorf("FindTable(missing) returned non-nil: %v", missing)
	}
}

func TestSchemaTemplate_ColumnTypesFromProto(t *testing.T) {
	t.Parallel()
	tmpl := buildTestTemplate(t)

	order, _ := tmpl.FindTable("Order")
	cols := order.Columns()

	byName := make(map[string]api.Column, len(cols))
	for _, c := range cols {
		byName[c.MetadataName()] = c
	}

	// Spot-check representative types. All proto fields are nullable
	// in the bridge (matches Java).
	assertColType(t, byName, "order_id", api.NewLongType(true))
	assertColType(t, byName, "price", api.NewIntegerType(true))
	assertColType(t, byName, "vector_data", api.NewBytesType(true))

	// Repeated field should become an Array.
	tags := byName["tags"]
	if tags == nil {
		t.Fatal("tags column missing")
	}
	arr, ok := tags.DataType().(*api.ArrayType)
	if !ok {
		t.Fatalf("tags.DataType = %T, want *ArrayType", tags.DataType())
	}
	if _, ok := arr.ElementType().(*api.StringType); !ok {
		t.Errorf("tags element type = %T, want *StringType", arr.ElementType())
	}

	// Nested message field -> StructType.
	flower := byName["flower"]
	if flower == nil {
		t.Fatal("flower column missing")
	}
	if _, ok := flower.DataType().(*api.StructType); !ok {
		t.Errorf("flower.DataType = %T, want *StructType", flower.DataType())
	}
}

func TestSchemaTemplate_EnumFieldIncludesValues(t *testing.T) {
	t.Parallel()
	tmpl := buildTestTemplate(t)
	order, _ := tmpl.FindTable("Order")

	// Flower.color is an enum Color{RED,BLUE,YELLOW,PINK}.
	var flowerCol api.Column
	for _, c := range order.Columns() {
		if c.MetadataName() == "flower" {
			flowerCol = c
			break
		}
	}
	if flowerCol == nil {
		t.Fatal("flower column missing")
	}
	st, ok := flowerCol.DataType().(*api.StructType)
	if !ok {
		t.Fatalf("flower DataType = %T", flowerCol.DataType())
	}
	var colorField *api.StructField
	for i := 0; i < st.NumFields(); i++ {
		f := st.Field(i)
		if f.Name() == "color" {
			colorField = &f
			break
		}
	}
	if colorField == nil {
		t.Fatal("color field missing from Flower struct")
	}
	enumT, ok := colorField.Type().(*api.EnumType)
	if !ok {
		t.Fatalf("color Type = %T, want *EnumType", colorField.Type())
	}
	if enumT.Name() != "Color" {
		t.Errorf("enum name = %q, want Color", enumT.Name())
	}
	// Values should contain RED(1), BLUE(2), YELLOW(3), PINK(4).
	values := enumT.Values()
	if len(values) != 4 {
		t.Fatalf("enum values len = %d, want 4: %v", len(values), values)
	}
}

func TestSchemaTemplate_TableStructDataType(t *testing.T) {
	t.Parallel()
	tmpl := buildTestTemplate(t)
	order, _ := tmpl.FindTable("Order")
	st := order.StructDataType()
	if st == nil {
		t.Fatal("StructDataType nil")
	}
	if st.Name() != "Order" {
		t.Errorf("struct name = %q", st.Name())
	}
	if st.NumFields() != len(order.Columns()) {
		t.Errorf("struct fields = %d, table columns = %d", st.NumFields(), len(order.Columns()))
	}
	// Java compliance: RecordLayerTable.getDatatype() emits nullable=true.
	if !st.IsNullable() {
		t.Error("table StructDataType should be nullable (matches Java)")
	}
}

func TestSchemaTemplate_ViewsAndRoutinesEmpty(t *testing.T) {
	t.Parallel()
	tmpl := buildTestTemplate(t)

	v, err := tmpl.Views()
	if err != nil || v != nil {
		t.Errorf("Views() = (%v, %v), want (nil, nil)", v, err)
	}
	view, err := tmpl.FindView("X")
	if err != nil || view != nil {
		t.Errorf("FindView() = (%v, %v), want (nil, nil)", view, err)
	}
	r, err := tmpl.InvokedRoutines()
	if err != nil || r != nil {
		t.Errorf("InvokedRoutines() = (%v, %v), want (nil, nil)", r, err)
	}
	routine, err := tmpl.FindInvokedRoutine("X")
	if err != nil || routine != nil {
		t.Errorf("FindInvokedRoutine() = (%v, %v), want (nil, nil)", routine, err)
	}
	tr, err := tmpl.TemporaryInvokedRoutines()
	if err != nil || tr != nil {
		t.Errorf("TemporaryInvokedRoutines() = (%v, %v), want (nil, nil)", tr, err)
	}
	s, err := tmpl.TransactionBoundMetadataAsString()
	if err != nil || s != "" {
		t.Errorf("TransactionBoundMetadataAsString() = (%q, %v), want (\"\", nil)", s, err)
	}
}

func TestSchemaTemplate_IndexesWithSecondaryIndex(t *testing.T) {
	t.Parallel()
	b := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	b.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	b.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	b.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	b.AddIndex("Order", recordlayer.NewIndex("order_by_price", recordlayer.Field("price")))
	md, err := b.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	tmpl, err := NewRecordLayerSchemaTemplate("demo", md)
	if err != nil {
		t.Fatalf("NewRecordLayerSchemaTemplate: %v", err)
	}

	names, err := tmpl.Indexes()
	if err != nil {
		t.Fatalf("Indexes(): %v", err)
	}
	expectContains(t, names, "order_by_price")

	mapping, err := tmpl.TableIndexMapping()
	if err != nil {
		t.Fatalf("TableIndexMapping: %v", err)
	}
	orderIdx := mapping["Order"]
	expectContains(t, orderIdx, "order_by_price")
	// Customer has no indexes — mapping should exist with empty slice.
	if _, ok := mapping["Customer"]; !ok {
		t.Error("Customer missing from TableIndexMapping")
	}

	order, _ := tmpl.FindTable("Order")
	idx := findIndex(order.Indexes(), "order_by_price")
	if idx == nil {
		t.Fatal("order_by_price index missing from Order")
	}
	if idx.IndexType() != recordlayer.IndexTypeValue {
		t.Errorf("IndexType = %q, want VALUE", idx.IndexType())
	}
	if idx.TableName() != "Order" {
		t.Errorf("TableName = %q, want Order", idx.TableName())
	}
	if idx.IsUnique() {
		t.Error("IsUnique = true, want false (no UNIQUE option)")
	}
	// IsSparse follows Java's predicate != null rule. order_by_price
	// is a plain VALUE index with no predicate, so it's NOT sparse.
	if idx.IsSparse() {
		t.Error("IsSparse = true, want false (no predicate on this index)")
	}
}

func TestSchemaTemplate_IndexIsSparseWhenPredicateSet(t *testing.T) {
	t.Parallel()
	// Java's RecordLayerIndex.isSparse() returns predicate != null.
	// Build an index with a predicate and assert IsSparse reports true.
	b := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	b.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	b.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	b.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))

	idx := recordlayer.NewIndex("order_by_price_sparse", recordlayer.Field("price"))
	// Any non-nil predicate makes the index sparse. Simplest is a
	// function that matches everything — we just need Predicate != nil.
	idx.Predicate = func(proto.Message) bool { return true }
	b.AddIndex("Order", idx)

	md, err := b.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	tmpl, err := NewRecordLayerSchemaTemplate("demo", md)
	if err != nil {
		t.Fatalf("NewRecordLayerSchemaTemplate: %v", err)
	}
	order, _ := tmpl.FindTable("Order")
	sparseIdx := findIndex(order.Indexes(), "order_by_price_sparse")
	if sparseIdx == nil {
		t.Fatal("order_by_price_sparse missing from Order")
	}
	if !sparseIdx.IsSparse() {
		t.Error("IsSparse = false, want true (predicate set)")
	}
}

func TestSchemaTemplate_UniversalIndexNotDuplicatedIntoEachTable(t *testing.T) {
	t.Parallel()
	// Java compliance: each RecordLayerTable only carries its own
	// per-type indexes (and multi-type ones that include it).
	// Universal indexes are reachable via Indexes() and
	// GetAllIndexes() but must NOT appear in table.Indexes() for every
	// table. Matches RecordMetadataDeserializer.generateTableBuilder.
	b := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	b.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	b.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	b.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	universal := recordlayer.NewCountIndex("universal_idx", recordlayer.Ungrouped(recordlayer.EmptyKey()))
	b.AddUniversalIndex(universal)
	md, err := b.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	tmpl, err := NewRecordLayerSchemaTemplate("demo", md)
	if err != nil {
		t.Fatalf("NewRecordLayerSchemaTemplate: %v", err)
	}

	// Indexes() is a template-wide flat list — universal SHOULD appear.
	names, _ := tmpl.Indexes()
	expectContains(t, names, "universal_idx")

	// Per-table — universal MUST NOT appear (Java divergence fix).
	for _, tbl := range mustTables(t, tmpl) {
		for _, idx := range tbl.Indexes() {
			if idx.MetadataName() == "universal_idx" {
				t.Errorf("universal_idx appeared on table %q — violates Java compliance", tbl.MetadataName())
			}
		}
	}
}

func mustTables(t *testing.T, tmpl *RecordLayerSchemaTemplate) []api.Table {
	t.Helper()
	tables, err := tmpl.Tables()
	if err != nil {
		t.Fatalf("Tables: %v", err)
	}
	return tables
}

func TestSchemaTemplate_GenerateSchema(t *testing.T) {
	t.Parallel()
	tmpl := buildTestTemplate(t)

	schema := tmpl.GenerateSchema("db1", "public")
	if schema.MetadataName() != "public" {
		t.Errorf("MetadataName = %q, want public", schema.MetadataName())
	}
	if schema.DatabaseName() != "db1" {
		t.Errorf("DatabaseName = %q, want db1", schema.DatabaseName())
	}
	if schema.SchemaTemplate() != tmpl {
		t.Error("SchemaTemplate() did not point to source template")
	}
}

func TestSchema_DelegatesToTemplate(t *testing.T) {
	t.Parallel()
	// Matches Java Schema.java default methods: getTables / getViews /
	// getIndexes / getInvokedRoutines all delegate to the template.
	tmpl := buildTestTemplate(t)
	schema := tmpl.GenerateSchema("db1", "public")

	wantTables, _ := tmpl.Tables()
	gotTables, err := schema.Tables()
	if err != nil {
		t.Fatalf("schema.Tables: %v", err)
	}
	if len(gotTables) != len(wantTables) {
		t.Errorf("schema.Tables len = %d, template has %d", len(gotTables), len(wantTables))
	}

	// Views / InvokedRoutines are empty in this bridge — verify
	// delegation still returns (nil, nil).
	views, err := schema.Views()
	if err != nil || views != nil {
		t.Errorf("schema.Views = (%v, %v), want (nil, nil)", views, err)
	}
	routines, err := schema.InvokedRoutines()
	if err != nil || routines != nil {
		t.Errorf("schema.InvokedRoutines = (%v, %v), want (nil, nil)", routines, err)
	}

	// Schema.Indexes returns (table → index names), NOT a flat list —
	// the Java shape.
	wantMap, _ := tmpl.TableIndexMapping()
	gotMap, err := schema.Indexes()
	if err != nil {
		t.Fatalf("schema.Indexes: %v", err)
	}
	if len(gotMap) != len(wantMap) {
		t.Errorf("schema.Indexes len = %d, template mapping len = %d", len(gotMap), len(wantMap))
	}
}

func TestSchemaTemplate_Visitor(t *testing.T) {
	t.Parallel()

	// Build with a secondary index so the cascade visits an Index too —
	// the default demo template has no indexes, which would make an
	// index-cascade regression silent.
	b := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	b.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	b.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	b.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	b.AddIndex("Order", recordlayer.NewIndex("order_by_price", recordlayer.Field("price")))
	md, err := b.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	tmpl, err := NewRecordLayerSchemaTemplate("demo", md)
	if err != nil {
		t.Fatalf("NewRecordLayerSchemaTemplate: %v", err)
	}

	v := &countingVisitor{}
	tmpl.Accept(v)

	// Java's RecordLayerSchemaTemplate.accept(): start → visit →
	// cascade tables (which cascade indexes + columns) → cascade
	// routines → cascade views → finish.
	if v.startTemplate != 1 || v.visitTemplate != 1 || v.finishTemplate != 1 {
		t.Errorf("schema template visits: start=%d visit=%d finish=%d, want 1/1/1",
			v.startTemplate, v.visitTemplate, v.finishTemplate)
	}
	tables, _ := tmpl.Tables()
	if v.tables != len(tables) {
		t.Errorf("table visits = %d, want %d (cascade)", v.tables, len(tables))
	}
	if v.columns == 0 {
		t.Error("visitor never saw a column — tables didn't cascade into columns")
	}
	if v.indexes == 0 {
		t.Error("visitor never saw an index — tables didn't cascade into indexes")
	}
}

// ---- helpers ----

type countingVisitor struct {
	startTemplate  int
	visitTemplate  int
	finishTemplate int
	tables         int
	columns        int
	indexes        int
}

func (c *countingVisitor) VisitTable(_ api.Table)                        { c.tables++ }
func (c *countingVisitor) VisitColumn(_ api.Column)                      { c.columns++ }
func (c *countingVisitor) StartVisitSchemaTemplate(_ api.SchemaTemplate) { c.startTemplate++ }
func (c *countingVisitor) VisitSchemaTemplate(_ api.SchemaTemplate)      { c.visitTemplate++ }
func (c *countingVisitor) FinishVisitSchemaTemplate(_ api.SchemaTemplate) {
	c.finishTemplate++
}
func (c *countingVisitor) VisitSchema(_ api.Schema)                 {}
func (c *countingVisitor) VisitIndex(_ api.Index)                   { c.indexes++ }
func (c *countingVisitor) VisitInvokedRoutine(_ api.InvokedRoutine) {}
func (c *countingVisitor) VisitView(_ api.View)                     {}

func tableNames(tables []api.Table) []string {
	names := make([]string, 0, len(tables))
	for _, t := range tables {
		names = append(names, t.MetadataName())
	}
	return names
}

func expectContains(t *testing.T, haystack []string, needles ...string) {
	t.Helper()
	set := make(map[string]struct{}, len(haystack))
	for _, s := range haystack {
		set[s] = struct{}{}
	}
	for _, n := range needles {
		if _, ok := set[n]; !ok {
			t.Errorf("expected %q in %v", n, haystack)
		}
	}
}

func findIndex(indexes []api.Index, name string) api.Index {
	for _, i := range indexes {
		if i.MetadataName() == name {
			return i
		}
	}
	return nil
}

func assertColType(t *testing.T, byName map[string]api.Column, colName string, want api.DataType) {
	t.Helper()
	c := byName[colName]
	if c == nil {
		t.Errorf("column %q missing", colName)
		return
	}
	if !c.DataType().Equal(want) {
		t.Errorf("%s: DataType = %v, want %v", colName, c.DataType(), want)
	}
}

func TestBuilder_BasicTemplate(t *testing.T) {
	t.Parallel()
	tmpl, err := NewSchemaTemplateBuilder().
		SetName("test_template").
		SetVersion(1).
		AddTable("Order", []ColumnSpec{
			NewColumnSpec("order_id", api.NewLongType(false), 1),
			NewColumnSpec("customer_id", api.NewLongType(false), 2),
			NewColumnSpec("amount", api.NewDoubleType(true), 3),
		}, []string{"order_id"}).
		Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if tmpl.MetadataName() != "test_template" {
		t.Errorf("MetadataName = %q, want %q", tmpl.MetadataName(), "test_template")
	}
	tables, err := tmpl.Tables()
	if err != nil {
		t.Fatalf("Tables: %v", err)
	}
	if len(tables) != 1 {
		t.Fatalf("len(Tables) = %d, want 1", len(tables))
	}
	if tables[0].MetadataName() != "Order" {
		t.Errorf("table name = %q, want %q", tables[0].MetadataName(), "Order")
	}
	cols := tables[0].Columns()
	if len(cols) != 3 {
		t.Errorf("len(Columns) = %d, want 3", len(cols))
	}
}

func TestBuilder_MultiTable(t *testing.T) {
	t.Parallel()
	tmpl, err := NewSchemaTemplateBuilder().
		SetName("multi_tmpl").
		SetIntermingleTables(true).
		AddTable("Foo", []ColumnSpec{
			NewColumnSpec("id", api.NewLongType(false), 1),
			NewColumnSpec("name", api.NewStringType(true), 2),
		}, []string{"id"}).
		AddTable("Bar", []ColumnSpec{
			NewColumnSpec("bar_id", api.NewIntegerType(false), 1),
		}, []string{"bar_id"}).
		Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	tables, err := tmpl.Tables()
	if err != nil {
		t.Fatalf("Tables: %v", err)
	}
	if len(tables) != 2 {
		t.Errorf("len(Tables) = %d, want 2", len(tables))
	}
}

func TestBuilder_EmptyTablesError(t *testing.T) {
	t.Parallel()
	_, err := NewSchemaTemplateBuilder().SetName("empty").Build()
	if err == nil {
		t.Fatal("expected error for empty template, got nil")
	}
}

func TestBuilder_EmptyNameError(t *testing.T) {
	t.Parallel()
	_, err := NewSchemaTemplateBuilder().
		AddTable("T", []ColumnSpec{NewColumnSpec("id", api.NewLongType(false), 1)}, []string{"id"}).
		Build()
	if err == nil {
		t.Fatal("expected error for missing name, got nil")
	}
}

func TestBuilder_EmptyPrimaryKeyError(t *testing.T) {
	t.Parallel()
	_, err := NewSchemaTemplateBuilder().
		SetName("no_pk").
		AddTable("T", []ColumnSpec{NewColumnSpec("id", api.NewLongType(false), 1)}, nil).
		Build()
	if err == nil {
		t.Fatal("expected error for empty primary key, got nil")
	}
}

func TestBuilder_AllColumnTypes(t *testing.T) {
	t.Parallel()
	cols := []ColumnSpec{
		NewColumnSpec("b", api.NewBooleanType(true), 1),
		NewColumnSpec("i", api.NewIntegerType(false), 2),
		NewColumnSpec("l", api.NewLongType(false), 3),
		NewColumnSpec("f", api.NewFloatType(true), 4),
		NewColumnSpec("d", api.NewDoubleType(true), 5),
		NewColumnSpec("s", api.NewStringType(true), 6),
		NewColumnSpec("by", api.NewBytesType(true), 7),
	}
	_, err := NewSchemaTemplateBuilder().
		SetName("all_types").
		AddTable("T", cols, []string{"l"}).
		Build()
	if err != nil {
		t.Fatalf("Build with all column types: %v", err)
	}
}

func TestBuilder_NullableVsNotNullable(t *testing.T) {
	t.Parallel()
	tmpl, err := NewSchemaTemplateBuilder().
		SetName("nullable_test").
		AddTable("T", []ColumnSpec{
			NewColumnSpec("id", api.NewLongType(false), 1),   // NOT NULL → REQUIRED in proto2
			NewColumnSpec("opt", api.NewStringType(true), 2), // NULL → OPTIONAL in proto2
		}, []string{"id"}).
		Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	tables, err := tmpl.Tables()
	if err != nil {
		t.Fatalf("Tables: %v", err)
	}
	cols := tables[0].Columns()
	if len(cols) != 2 {
		t.Fatalf("len(Columns) = %d, want 2", len(cols))
	}
	if cols[0].DataType().IsNullable() {
		t.Errorf("id should be non-nullable")
	}
	if !cols[1].DataType().IsNullable() {
		t.Errorf("opt should be nullable")
	}
}

func TestBuilder_WithValueIndex(t *testing.T) {
	t.Parallel()
	tmpl, err := NewSchemaTemplateBuilder().
		SetName("indexed").
		AddTable("Order", []ColumnSpec{
			NewColumnSpec("order_id", api.NewLongType(false), 1),
			NewColumnSpec("customer_id", api.NewLongType(true), 2),
			NewColumnSpec("total", api.NewLongType(true), 3),
		}, []string{"order_id"}).
		AddIndex("Order", "by_customer", []string{"customer_id"}, false).
		Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	tables, err := tmpl.Tables()
	if err != nil {
		t.Fatalf("Tables: %v", err)
	}
	if len(tables) != 1 {
		t.Fatalf("len(tables) = %d, want 1", len(tables))
	}
	idxs := tables[0].Indexes()
	if len(idxs) != 1 {
		t.Fatalf("len(indexes) = %d, want 1", len(idxs))
	}
	if idxs[0].MetadataName() != "by_customer" {
		t.Errorf("index name = %q, want %q", idxs[0].MetadataName(), "by_customer")
	}
}

func TestBuilder_MultiColumnIndex(t *testing.T) {
	t.Parallel()
	tmpl, err := NewSchemaTemplateBuilder().
		SetName("multi_idx").
		AddTable("T", []ColumnSpec{
			NewColumnSpec("a", api.NewStringType(true), 1),
			NewColumnSpec("b", api.NewStringType(true), 2),
			NewColumnSpec("id", api.NewLongType(false), 3),
		}, []string{"id"}).
		AddIndex("T", "by_ab", []string{"a", "b"}, false).
		Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	tables, err := tmpl.Tables()
	if err != nil {
		t.Fatalf("Tables: %v", err)
	}
	idxs := tables[0].Indexes()
	if len(idxs) != 1 {
		t.Fatalf("len(indexes) = %d, want 1", len(idxs))
	}
}

func TestBuilder_IndexOnUnknownTableFails(t *testing.T) {
	t.Parallel()
	_, err := NewSchemaTemplateBuilder().
		SetName("bad_idx").
		AddTable("T", []ColumnSpec{NewColumnSpec("id", api.NewLongType(false), 1)}, []string{"id"}).
		AddIndex("NoSuchTable", "idx", []string{"id"}, false).
		Build()
	if err == nil {
		t.Fatal("expected error for index on unknown table, got nil")
	}
	if !strings.Contains(err.Error(), "unknown table") {
		t.Fatalf("want 'unknown table' in error, got %v", err)
	}
}

func TestBuilder_IndexOnUnknownColumnFails(t *testing.T) {
	t.Parallel()
	_, err := NewSchemaTemplateBuilder().
		SetName("bad_col_idx").
		AddTable("T", []ColumnSpec{NewColumnSpec("id", api.NewLongType(false), 1)}, []string{"id"}).
		AddIndex("T", "idx", []string{"nonexistent"}, false).
		Build()
	if err == nil {
		t.Fatal("expected error for index on unknown column, got nil")
	}
	if !strings.Contains(err.Error(), "column") {
		t.Fatalf("want 'column' in error, got %v", err)
	}
}
