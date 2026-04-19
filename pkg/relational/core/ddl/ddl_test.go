package ddl_test

import (
	"errors"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/catalog"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/ddl"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/metadata"
	"github.com/onsi/gomega"
)

// buildTemplate creates a simple named schema template using the demo proto.
func buildTemplate(t *testing.T, name string, version int) api.SchemaTemplate {
	t.Helper()
	b := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	b.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	b.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	b.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	md, err := b.Build()
	if err != nil {
		t.Fatalf("build metadata: %v", err)
	}
	tmpl, err := metadata.NewRecordLayerSchemaTemplateWithVersion(name, md, version)
	if err != nil {
		t.Fatalf("NewRecordLayerSchemaTemplateWithVersion: %v", err)
	}
	return tmpl
}

func newEnv(t *testing.T) (api.StoreCatalog, api.Transaction, *ddl.RecordLayerMetadataOperationsFactory) {
	t.Helper()
	cat := catalog.NewInMemoryStoreCatalog()
	txn := catalog.NewInMemoryTransaction()
	f := ddl.NewRecordLayerMetadataOperationsFactory(cat)
	return cat, txn, f
}

func TestCreateDatabase(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	cat, txn, f := newEnv(t)

	g.Expect(f.CreateDatabase("/test", api.Options{}).Execute(txn)).To(gomega.Succeed())

	exists, err := cat.DoesDatabaseExist(txn, "/test")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(exists).To(gomega.BeTrue())
}

func TestCreateDatabase_Duplicate(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	_, txn, f := newEnv(t)

	g.Expect(f.CreateDatabase("/test", api.Options{}).Execute(txn)).To(gomega.Succeed())
	err := f.CreateDatabase("/test", api.Options{}).Execute(txn)
	g.Expect(err).To(gomega.HaveOccurred())
	var apiErr *api.Error
	g.Expect(err).To(gomega.BeAssignableToTypeOf(apiErr))
}

func TestDropDatabase(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	cat, txn, f := newEnv(t)

	g.Expect(f.CreateDatabase("/test", api.Options{}).Execute(txn)).To(gomega.Succeed())
	g.Expect(f.DropDatabase("/test", true, api.Options{}).Execute(txn)).To(gomega.Succeed())

	exists, err := cat.DoesDatabaseExist(txn, "/test")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(exists).To(gomega.BeFalse())
}

func TestDropDatabase_ThrowIfNotExist(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	_, txn, f := newEnv(t)

	g.Expect(f.DropDatabase("/nope", true, api.Options{}).Execute(txn)).NotTo(gomega.Succeed())
	g.Expect(f.DropDatabase("/nope", false, api.Options{}).Execute(txn)).To(gomega.Succeed())
}

func TestDropDatabase_ProtectedSys(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	_, txn, f := newEnv(t)

	err := f.DropDatabase("/__SYS", false, api.Options{}).Execute(txn)
	g.Expect(err).To(gomega.HaveOccurred())
	var apiErr *api.Error
	g.Expect(err).To(gomega.BeAssignableToTypeOf(apiErr))
}

func TestDropDatabase_DropsSchemasFirst(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	cat, txn, f := newEnv(t)

	tmpl := buildTemplate(t, "T1", 1)
	g.Expect(f.SaveSchemaTemplate(tmpl, api.Options{}).Execute(txn)).To(gomega.Succeed())
	g.Expect(f.CreateDatabase("/db1", api.Options{}).Execute(txn)).To(gomega.Succeed())
	g.Expect(f.CreateSchema("/db1", "s1", "T1", api.Options{}).Execute(txn)).To(gomega.Succeed())

	g.Expect(f.DropDatabase("/db1", true, api.Options{}).Execute(txn)).To(gomega.Succeed())

	exists, err := cat.DoesDatabaseExist(txn, "/db1")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(exists).To(gomega.BeFalse())
}

func TestCreateSchema(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	cat, txn, f := newEnv(t)

	tmpl := buildTemplate(t, "T1", 1)
	g.Expect(f.SaveSchemaTemplate(tmpl, api.Options{}).Execute(txn)).To(gomega.Succeed())
	g.Expect(f.CreateDatabase("/db1", api.Options{}).Execute(txn)).To(gomega.Succeed())
	g.Expect(f.CreateSchema("/db1", "s1", "T1", api.Options{}).Execute(txn)).To(gomega.Succeed())

	schema, err := cat.LoadSchema(txn, "/db1", "s1")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(schema.MetadataName()).To(gomega.Equal("s1"))
}

func TestCreateSchema_DatabaseNotExist(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	_, txn, f := newEnv(t)

	tmpl := buildTemplate(t, "T1", 1)
	g.Expect(f.SaveSchemaTemplate(tmpl, api.Options{}).Execute(txn)).To(gomega.Succeed())

	err := f.CreateSchema("/nodb", "s1", "T1", api.Options{}).Execute(txn)
	g.Expect(err).To(gomega.HaveOccurred())
}

func TestCreateSchema_AlreadyExists(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	_, txn, f := newEnv(t)

	tmpl := buildTemplate(t, "T1", 1)
	g.Expect(f.SaveSchemaTemplate(tmpl, api.Options{}).Execute(txn)).To(gomega.Succeed())
	g.Expect(f.CreateDatabase("/db1", api.Options{}).Execute(txn)).To(gomega.Succeed())
	g.Expect(f.CreateSchema("/db1", "s1", "T1", api.Options{}).Execute(txn)).To(gomega.Succeed())

	err := f.CreateSchema("/db1", "s1", "T1", api.Options{}).Execute(txn)
	g.Expect(err).To(gomega.HaveOccurred())
}

func TestDropSchema(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	cat, txn, f := newEnv(t)

	tmpl := buildTemplate(t, "T1", 1)
	g.Expect(f.SaveSchemaTemplate(tmpl, api.Options{}).Execute(txn)).To(gomega.Succeed())
	g.Expect(f.CreateDatabase("/db1", api.Options{}).Execute(txn)).To(gomega.Succeed())
	g.Expect(f.CreateSchema("/db1", "s1", "T1", api.Options{}).Execute(txn)).To(gomega.Succeed())
	g.Expect(f.DropSchema("/db1", "s1", api.Options{}).Execute(txn)).To(gomega.Succeed())

	_, err := cat.LoadSchema(txn, "/db1", "s1")
	g.Expect(err).To(gomega.HaveOccurred())
}

func TestDropSchema_ProtectedSys(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	_, txn, f := newEnv(t)

	err := f.DropSchema("/__SYS", "CATALOG", api.Options{}).Execute(txn)
	g.Expect(err).To(gomega.HaveOccurred())
}

func TestSaveAndDropSchemaTemplate(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	cat, txn, f := newEnv(t)

	tmpl := buildTemplate(t, "T1", 1)
	g.Expect(f.SaveSchemaTemplate(tmpl, api.Options{}).Execute(txn)).To(gomega.Succeed())

	loaded, err := cat.SchemaTemplateCatalog().LoadSchemaTemplate(txn, "T1")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(loaded.MetadataName()).To(gomega.Equal("T1"))

	g.Expect(f.DropSchemaTemplate("T1", true, api.Options{}).Execute(txn)).To(gomega.Succeed())

	_, err = cat.SchemaTemplateCatalog().LoadSchemaTemplate(txn, "T1")
	g.Expect(err).To(gomega.HaveOccurred())
}

func TestDropSchemaTemplate_NotExist(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	_, txn, f := newEnv(t)

	// throwIfDoesNotExist=true → error.
	g.Expect(f.DropSchemaTemplate("NoSuchTemplate", true, api.Options{}).Execute(txn)).NotTo(gomega.Succeed())
	// throwIfDoesNotExist=false → no-op, no error.
	g.Expect(f.DropSchemaTemplate("NoSuchTemplate", false, api.Options{}).Execute(txn)).To(gomega.Succeed())
}

func TestCreateSchema_MissingTemplate(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	_, txn, f := newEnv(t)

	g.Expect(f.CreateDatabase("/db", api.Options{}).Execute(txn)).To(gomega.Succeed())
	// Template "ghost" was never saved — CreateSchema must fail.
	g.Expect(f.CreateSchema("/db", "s1", "ghost", api.Options{}).Execute(txn)).NotTo(gomega.Succeed())
}

// --- Schema evolution validator tests ---

type tableSpec struct {
	name   string
	cols   []metadata.ColumnSpec
	pkCols []string
}

func evolutionTemplate(t *testing.T, name string, version int, tables []tableSpec) api.SchemaTemplate {
	t.Helper()
	b := metadata.NewSchemaTemplateBuilder().SetName(name).SetVersion(version).SetIntermingleTables(true)
	for _, ts := range tables {
		b = b.AddTable(ts.name, ts.cols, ts.pkCols)
	}
	tmpl, err := b.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return tmpl
}

func ordersTable(extra ...metadata.ColumnSpec) []metadata.ColumnSpec {
	base := []metadata.ColumnSpec{
		metadata.NewColumnSpec("order_id", api.NewLongType(false), 1),
		metadata.NewColumnSpec("amount", api.NewDoubleType(true), 2),
	}
	return append(base, extra...)
}

func TestSchemaEvolution_AddColumn_Allowed(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	_, txn, f := newEnv(t)

	v1 := evolutionTemplate(t, "evo", 1, []tableSpec{{
		name: "Order", cols: ordersTable(), pkCols: []string{"order_id"},
	}})
	v2 := evolutionTemplate(t, "evo", 2, []tableSpec{{
		name:   "Order",
		cols:   ordersTable(metadata.NewColumnSpec("note", api.NewStringType(true), 3)),
		pkCols: []string{"order_id"},
	}})

	g.Expect(f.SaveSchemaTemplate(v1, api.Options{}).Execute(txn)).To(gomega.Succeed())
	g.Expect(f.SaveSchemaTemplate(v2, api.Options{}).Execute(txn)).To(gomega.Succeed())
}

func TestSchemaEvolution_RemoveColumn_Rejected(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	_, txn, f := newEnv(t)

	v1 := evolutionTemplate(t, "evo", 1, []tableSpec{{
		name:   "Order",
		cols:   ordersTable(metadata.NewColumnSpec("note", api.NewStringType(true), 3)),
		pkCols: []string{"order_id"},
	}})
	v2 := evolutionTemplate(t, "evo", 2, []tableSpec{{
		name: "Order", cols: ordersTable(), pkCols: []string{"order_id"},
	}})

	g.Expect(f.SaveSchemaTemplate(v1, api.Options{}).Execute(txn)).To(gomega.Succeed())
	err := f.SaveSchemaTemplate(v2, api.Options{}).Execute(txn)
	g.Expect(err).To(gomega.HaveOccurred())
	var apiErr *api.Error
	g.Expect(err).To(gomega.BeAssignableToTypeOf(apiErr))
}

func TestSchemaEvolution_RemoveTable_Rejected(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	_, txn, f := newEnv(t)

	v1 := evolutionTemplate(t, "evo", 1, []tableSpec{
		{name: "Order", cols: ordersTable(), pkCols: []string{"order_id"}},
		{name: "Customer", cols: []metadata.ColumnSpec{metadata.NewColumnSpec("id", api.NewLongType(false), 1)}, pkCols: []string{"id"}},
	})
	v2 := evolutionTemplate(t, "evo", 2, []tableSpec{{
		name: "Order", cols: ordersTable(), pkCols: []string{"order_id"},
	}})

	g.Expect(f.SaveSchemaTemplate(v1, api.Options{}).Execute(txn)).To(gomega.Succeed())
	err := f.SaveSchemaTemplate(v2, api.Options{}).Execute(txn)
	g.Expect(err).To(gomega.HaveOccurred())
	var apiErr *api.Error
	g.Expect(err).To(gomega.BeAssignableToTypeOf(apiErr))
}

func TestSchemaEvolution_ChangeColumnType_Rejected(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	_, txn, f := newEnv(t)

	v1 := evolutionTemplate(t, "evo", 1, []tableSpec{{
		name: "Order", cols: ordersTable(), pkCols: []string{"order_id"},
	}})
	// Change "amount" from DOUBLE to STRING — must be rejected.
	v2 := evolutionTemplate(t, "evo", 2, []tableSpec{{
		name: "Order",
		cols: []metadata.ColumnSpec{
			metadata.NewColumnSpec("order_id", api.NewLongType(false), 1),
			metadata.NewColumnSpec("amount", api.NewStringType(true), 2),
		},
		pkCols: []string{"order_id"},
	}})

	g.Expect(f.SaveSchemaTemplate(v1, api.Options{}).Execute(txn)).To(gomega.Succeed())
	err := f.SaveSchemaTemplate(v2, api.Options{}).Execute(txn)
	g.Expect(err).To(gomega.HaveOccurred())
	var apiErr *api.Error
	g.Expect(err).To(gomega.BeAssignableToTypeOf(apiErr))
}

func TestSchemaEvolution_ReorderColumns_Rejected(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	_, txn, f := newEnv(t)

	v1 := evolutionTemplate(t, "evo", 1, []tableSpec{{
		name: "Order", cols: ordersTable(), pkCols: []string{"order_id"},
	}})
	// Swap order_id and amount positions.
	v2 := evolutionTemplate(t, "evo", 2, []tableSpec{{
		name: "Order",
		cols: []metadata.ColumnSpec{
			metadata.NewColumnSpec("amount", api.NewDoubleType(true), 1),
			metadata.NewColumnSpec("order_id", api.NewLongType(false), 2),
		},
		pkCols: []string{"order_id"},
	}})

	g.Expect(f.SaveSchemaTemplate(v1, api.Options{}).Execute(txn)).To(gomega.Succeed())
	err := f.SaveSchemaTemplate(v2, api.Options{}).Execute(txn)
	g.Expect(err).To(gomega.HaveOccurred())
	var apiErr *api.Error
	g.Expect(err).To(gomega.BeAssignableToTypeOf(apiErr))
}

func TestSchemaEvolution_AddTable_Allowed(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	_, txn, f := newEnv(t)

	v1 := evolutionTemplate(t, "evo", 1, []tableSpec{{
		name: "Order", cols: ordersTable(), pkCols: []string{"order_id"},
	}})
	v2 := evolutionTemplate(t, "evo", 2, []tableSpec{
		{name: "Order", cols: ordersTable(), pkCols: []string{"order_id"}},
		{name: "Customer", cols: []metadata.ColumnSpec{metadata.NewColumnSpec("id", api.NewLongType(false), 1)}, pkCols: []string{"id"}},
	})

	g.Expect(f.SaveSchemaTemplate(v1, api.Options{}).Execute(txn)).To(gomega.Succeed())
	g.Expect(f.SaveSchemaTemplate(v2, api.Options{}).Execute(txn)).To(gomega.Succeed())
}

func TestSchemaEvolution_VersionRollback_Rejected(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	_, txn, f := newEnv(t)

	v2 := evolutionTemplate(t, "evo", 2, []tableSpec{{
		name: "Order", cols: ordersTable(), pkCols: []string{"order_id"},
	}})
	v1 := evolutionTemplate(t, "evo", 1, []tableSpec{{
		name: "Order", cols: ordersTable(), pkCols: []string{"order_id"},
	}})

	g.Expect(f.SaveSchemaTemplate(v2, api.Options{}).Execute(txn)).To(gomega.Succeed())
	err := f.SaveSchemaTemplate(v1, api.Options{}).Execute(txn)
	g.Expect(err).To(gomega.HaveOccurred())
	var apiErr *api.Error
	g.Expect(errors.As(err, &apiErr)).To(gomega.BeTrue())
	g.Expect(apiErr.Code).To(gomega.Equal(api.ErrCodeInvalidSchemaTemplate))
}

func TestSchemaEvolution_SameVersion_Rejected(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	_, txn, f := newEnv(t)

	v1 := evolutionTemplate(t, "evo", 1, []tableSpec{{
		name: "Order", cols: ordersTable(), pkCols: []string{"order_id"},
	}})

	g.Expect(f.SaveSchemaTemplate(v1, api.Options{}).Execute(txn)).To(gomega.Succeed())
	err := f.SaveSchemaTemplate(v1, api.Options{}).Execute(txn)
	g.Expect(err).To(gomega.HaveOccurred())
	var apiErr *api.Error
	g.Expect(errors.As(err, &apiErr)).To(gomega.BeTrue())
	g.Expect(apiErr.Code).To(gomega.Equal(api.ErrCodeInvalidSchemaTemplate))
}
