package api

import "testing"

// stubColumn / stubIndex / stubTable / stubSchemaTemplate implement
// the metadata interfaces minimally so we can verify the visitor
// dispatch shape. The real implementations live later under
// pkg/relational/core.

type stubColumn struct {
	name string
	dt   DataType
}

func (c *stubColumn) MetadataName() string { return c.name }
func (c *stubColumn) Accept(v Visitor)     { v.VisitColumn(c) }
func (c *stubColumn) DataType() DataType   { return c.dt }

type stubIndex struct {
	name, table, indexType string
	unique, sparse         bool
}

func (i *stubIndex) MetadataName() string { return i.name }
func (i *stubIndex) Accept(v Visitor)     { v.VisitIndex(i) }
func (i *stubIndex) TableName() string    { return i.table }
func (i *stubIndex) IndexType() string    { return i.indexType }
func (i *stubIndex) IsUnique() bool       { return i.unique }
func (i *stubIndex) IsSparse() bool       { return i.sparse }

type stubTable struct {
	name    string
	indexes []Index
	columns []Column
	structT *StructType
}

func (t *stubTable) MetadataName() string        { return t.name }
func (t *stubTable) Accept(v Visitor)            { VisitTableTree(t, v) }
func (t *stubTable) Indexes() []Index            { return t.indexes }
func (t *stubTable) Columns() []Column           { return t.columns }
func (t *stubTable) StructDataType() *StructType { return t.structT }

// recordingVisitor captures the order in which nodes are visited.
type recordingVisitor struct {
	order []string
}

func (r *recordingVisitor) record(s string) { r.order = append(r.order, s) }

func (r *recordingVisitor) VisitTable(t Table)   { r.record("table:" + t.MetadataName()) }
func (r *recordingVisitor) VisitColumn(c Column) { r.record("column:" + c.MetadataName()) }
func (r *recordingVisitor) StartVisitSchemaTemplate(_ SchemaTemplate) {
	r.record("startSchemaTemplate")
}
func (r *recordingVisitor) VisitSchemaTemplate(_ SchemaTemplate) { r.record("visitSchemaTemplate") }
func (r *recordingVisitor) FinishVisitSchemaTemplate(_ SchemaTemplate) {
	r.record("finishSchemaTemplate")
}
func (r *recordingVisitor) VisitSchema(s Schema) { r.record("schema:" + s.MetadataName()) }
func (r *recordingVisitor) VisitIndex(i Index)   { r.record("index:" + i.MetadataName()) }
func (r *recordingVisitor) VisitInvokedRoutine(rt InvokedRoutine) {
	r.record("routine:" + rt.MetadataName())
}
func (r *recordingVisitor) VisitView(vw View) { r.record("view:" + vw.MetadataName()) }

// stubSchemaTemplate tests SchemaTemplate visitor dispatch.
type stubSchemaTemplate struct{ name string }

func (s *stubSchemaTemplate) MetadataName() string { return s.name }
func (s *stubSchemaTemplate) Accept(v Visitor)     { VisitSchemaTemplateTree(s, v) }

// The rest of SchemaTemplate's methods panic — we do not exercise
// them in this test, and the interface assertion below catches a
// missing method at compile time.
func (s *stubSchemaTemplate) Version() int                    { return 0 }
func (s *stubSchemaTemplate) EnableLongRows() bool            { return false }
func (s *stubSchemaTemplate) StoreRowVersions() bool          { return false }
func (s *stubSchemaTemplate) IntermingleTables() bool         { return false }
func (s *stubSchemaTemplate) Tables() ([]Table, error)        { return nil, nil }
func (s *stubSchemaTemplate) Views() ([]View, error)          { return nil, nil }
func (s *stubSchemaTemplate) FindTable(string) (Table, error) { return nil, nil }
func (s *stubSchemaTemplate) FindView(string) (View, error)   { return nil, nil }
func (s *stubSchemaTemplate) TableIndexMapping() (map[string][]string, error) {
	return nil, nil
}

func (s *stubSchemaTemplate) Indexes() ([]string, error) { return nil, nil }
func (s *stubSchemaTemplate) InvokedRoutines() ([]InvokedRoutine, error) {
	return nil, nil
}

func (s *stubSchemaTemplate) FindInvokedRoutine(string) (InvokedRoutine, error) {
	return nil, nil
}

func (s *stubSchemaTemplate) TemporaryInvokedRoutines() ([]InvokedRoutine, error) {
	return nil, nil
}

func (s *stubSchemaTemplate) TransactionBoundMetadataAsString() (string, error) {
	return "", nil
}
func (s *stubSchemaTemplate) GenerateSchema(string, string) Schema { return nil }

func TestVisitTableTreeOrder(t *testing.T) {
	t.Parallel()
	tbl := &stubTable{
		name: "orders",
		indexes: []Index{
			&stubIndex{name: "order_idx_1", table: "orders", indexType: "VALUE"},
			&stubIndex{name: "order_idx_2", table: "orders", indexType: "COUNT"},
		},
		columns: []Column{
			&stubColumn{name: "id", dt: NewLongType(false)},
			&stubColumn{name: "price", dt: NewIntegerType(true)},
		},
	}

	v := &recordingVisitor{}
	tbl.Accept(v)

	// Java: VisitTable first, then indexes in order, then columns in order.
	want := []string{
		"table:orders",
		"index:order_idx_1",
		"index:order_idx_2",
		"column:id",
		"column:price",
	}
	if len(v.order) != len(want) {
		t.Fatalf("order len = %d, want %d: %v", len(v.order), len(want), v.order)
	}
	for i, w := range want {
		if v.order[i] != w {
			t.Errorf("order[%d] = %q, want %q", i, v.order[i], w)
		}
	}
}

func TestVisitSchemaTemplateTreeOrder(t *testing.T) {
	t.Parallel()
	s := &stubSchemaTemplate{name: "myTemplate"}
	v := &recordingVisitor{}
	s.Accept(v)
	want := []string{"startSchemaTemplate", "visitSchemaTemplate", "finishSchemaTemplate"}
	for i, w := range want {
		if i >= len(v.order) || v.order[i] != w {
			t.Errorf("order[%d] = %q, want %q (full: %v)", i, v.order[i], w, v.order)
		}
	}
}

// Compile-time: every interface's method set is satisfied by its stub.
var (
	_ Column         = (*stubColumn)(nil)
	_ Index          = (*stubIndex)(nil)
	_ Table          = (*stubTable)(nil)
	_ SchemaTemplate = (*stubSchemaTemplate)(nil)
)
