package metadata

import (
	"testing"

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
	if idx.IsSparse() {
		t.Error("IsSparse = true, want false (no sparse support yet)")
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
