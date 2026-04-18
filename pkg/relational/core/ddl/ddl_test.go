package ddl_test

import (
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
