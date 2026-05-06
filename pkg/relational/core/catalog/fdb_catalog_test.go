package catalog

import (
	"context"
	"errors"
	"log"
	"os"
	"testing"
	"time"

	"github.com/onsi/gomega"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/metadata"
	foundationdbtc "github.com/birdayz/fdb-record-layer-go/pkg/testcontainers/foundationdb"
)

var testFDB *recordlayer.FDBDatabase

func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	container, err := foundationdbtc.Run(ctx, "", foundationdbtc.WithAPIVersion(720))
	if err != nil {
		log.Fatalf("fdb catalog test: failed to start FDB container: %v", err)
	}

	clusterFile, err := container.ClusterFile(ctx)
	if err != nil {
		log.Fatalf("fdb catalog test: failed to get cluster file: %v", err)
	}

	tmpFile, err := os.CreateTemp("", "fdb_catalog_cluster_*.txt")
	if err != nil {
		log.Fatalf("fdb catalog test: failed to create temp file: %v", err)
	}
	if _, err := tmpFile.WriteString(clusterFile); err != nil {
		log.Fatalf("fdb catalog test: failed to write cluster file: %v", err)
	}
	tmpFile.Close()

	fdb.MustAPIVersion(720)
	rawDB, err := fdb.OpenDatabase(tmpFile.Name())
	if err != nil {
		log.Fatalf("fdb catalog test: failed to open FDB: %v", err)
	}
	testFDB = recordlayer.NewFDBDatabase(rawDB)

	code := m.Run()
	_ = container.Terminate(ctx)
	_ = os.Remove(tmpFile.Name())
	os.Exit(code)
}

// newFDBCatalogInSubspace opens an FDB-backed StoreCatalog rooted at a
// test-unique subspace so parallel tests don't interfere.
func newFDBCatalogInSubspace(t *testing.T) (*RecordLayerStoreCatalog, func(fn func(txn api.Transaction) error) error) {
	t.Helper()
	testSubspace := subspace.Sub([]byte("fdbcat-test"), []byte(t.Name()))
	cat, err := NewRecordLayerStoreCatalog(testSubspace)
	if err != nil {
		t.Fatalf("NewRecordLayerStoreCatalog: %v", err)
	}

	runTxn := func(fn func(txn api.Transaction) error) error {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, err := testFDB.Run(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
			return nil, fn(NewFDBTransaction(rctx))
		})
		return err
	}

	return cat, runTxn
}

// buildVersionedTemplate builds a named template at an explicit version.
func buildVersionedTemplate(t testing.TB, name string, version int) api.SchemaTemplate {
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

// TestFDB_DatabaseCRUD creates, checks existence, and reads back a database.
func TestFDB_DatabaseCRUD(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	cat, run := newFDBCatalogInSubspace(t)

	g.Expect(run(func(tx api.Transaction) error {
		ok, err := cat.DoesDatabaseExist(tx, "/mydb")
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(ok).To(gomega.BeFalse())
		return nil
	})).To(gomega.Succeed())

	g.Expect(run(func(tx api.Transaction) error {
		return cat.CreateDatabase(tx, "/mydb")
	})).To(gomega.Succeed())

	g.Expect(run(func(tx api.Transaction) error {
		ok, err := cat.DoesDatabaseExist(tx, "/mydb")
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(ok).To(gomega.BeTrue())
		return nil
	})).To(gomega.Succeed())
}

// TestFDB_ListDatabases ensures ListDatabases returns all persisted rows.
func TestFDB_ListDatabases(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	cat, run := newFDBCatalogInSubspace(t)

	g.Expect(run(func(tx api.Transaction) error {
		g.Expect(cat.CreateDatabase(tx, "/a")).To(gomega.Succeed())
		g.Expect(cat.CreateDatabase(tx, "/b")).To(gomega.Succeed())
		return nil
	})).To(gomega.Succeed())

	g.Expect(run(func(tx api.Transaction) error {
		rs, err := cat.ListDatabases(tx, nil)
		g.Expect(err).ToNot(gomega.HaveOccurred())
		defer rs.Close()
		var got []string
		for rs.Next() {
			id, _ := rs.String(1)
			got = append(got, id)
		}
		g.Expect(rs.Err()).ToNot(gomega.HaveOccurred())
		g.Expect(got).To(gomega.ConsistOf("/a", "/b"))
		return nil
	})).To(gomega.Succeed())
}

// TestFDB_TemplateCRUD exercises CreateTemplate / LoadSchemaTemplate /
// DoesSchemaTemplateExist / DeleteTemplate round-trip.
func TestFDB_TemplateCRUD(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	cat, run := newFDBCatalogInSubspace(t)
	tc := cat.SchemaTemplateCatalog()
	tmpl := buildVersionedTemplate(t, "fdb-tmpl", 1)

	g.Expect(run(func(tx api.Transaction) error {
		ok, err := tc.DoesSchemaTemplateExist(tx, "fdb-tmpl")
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(ok).To(gomega.BeFalse())
		return nil
	})).To(gomega.Succeed())

	g.Expect(run(func(tx api.Transaction) error {
		return tc.CreateTemplate(tx, tmpl)
	})).To(gomega.Succeed())

	g.Expect(run(func(tx api.Transaction) error {
		ok, err := tc.DoesSchemaTemplateExist(tx, "fdb-tmpl")
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(ok).To(gomega.BeTrue())
		return nil
	})).To(gomega.Succeed())

	g.Expect(run(func(tx api.Transaction) error {
		got, err := tc.LoadSchemaTemplate(tx, "fdb-tmpl")
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(got.MetadataName()).To(gomega.Equal("fdb-tmpl"))
		g.Expect(got.Version()).To(gomega.Equal(tmpl.Version()))
		return nil
	})).To(gomega.Succeed())

	g.Expect(run(func(tx api.Transaction) error {
		return tc.DeleteTemplate(tx, "fdb-tmpl", true)
	})).To(gomega.Succeed())

	g.Expect(run(func(tx api.Transaction) error {
		ok, err := tc.DoesSchemaTemplateExist(tx, "fdb-tmpl")
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(ok).To(gomega.BeFalse())
		return nil
	})).To(gomega.Succeed())
}

// TestFDB_TemplateVersioning verifies that LoadSchemaTemplate returns
// the latest version and LoadSchemaTemplateAtVersion returns exactly
// the requested one.
func TestFDB_TemplateVersioning(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	cat, run := newFDBCatalogInSubspace(t)
	tc := cat.SchemaTemplateCatalog()

	tmpl1 := buildVersionedTemplate(t, "versioned", 1)
	tmpl2 := buildVersionedTemplate(t, "versioned", 2)

	g.Expect(run(func(tx api.Transaction) error {
		g.Expect(tc.CreateTemplate(tx, tmpl1)).To(gomega.Succeed())
		return tc.CreateTemplate(tx, tmpl2)
	})).To(gomega.Succeed())

	g.Expect(run(func(tx api.Transaction) error {
		got, err := tc.LoadSchemaTemplate(tx, "versioned")
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(got.Version()).To(gomega.Equal(2))
		return nil
	})).To(gomega.Succeed())

	g.Expect(run(func(tx api.Transaction) error {
		got, err := tc.LoadSchemaTemplateAtVersion(tx, "versioned", 1)
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(got.Version()).To(gomega.Equal(1))
		return nil
	})).To(gomega.Succeed())
}

// TestFDB_TemplateDuplicateReturnsError: creating (name, version) twice
// returns ErrCodeDuplicateSchemaTemplate.
func TestFDB_TemplateDuplicateReturnsError(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	cat, run := newFDBCatalogInSubspace(t)
	tc := cat.SchemaTemplateCatalog()
	tmpl := buildVersionedTemplate(t, "dup-tmpl", 1)

	g.Expect(run(func(tx api.Transaction) error {
		return tc.CreateTemplate(tx, tmpl)
	})).To(gomega.Succeed())

	err := run(func(tx api.Transaction) error {
		return tc.CreateTemplate(tx, tmpl)
	})
	var apiErr *api.Error
	g.Expect(errors.As(err, &apiErr)).To(gomega.BeTrue())
	g.Expect(apiErr.Code).To(gomega.Equal(api.ErrCodeDuplicateSchemaTemplate))
}

// TestFDB_SchemaCRUD exercises the full schema lifecycle on real FDB.
func TestFDB_SchemaCRUD(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	cat, run := newFDBCatalogInSubspace(t)
	tc := cat.SchemaTemplateCatalog()
	tmpl := buildVersionedTemplate(t, "schema-tmpl", 1)

	g.Expect(run(func(tx api.Transaction) error {
		return tc.CreateTemplate(tx, tmpl)
	})).To(gomega.Succeed())

	g.Expect(run(func(tx api.Transaction) error {
		ok, err := cat.DoesSchemaExist(tx, "/db", "pub")
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(ok).To(gomega.BeFalse())
		return nil
	})).To(gomega.Succeed())

	g.Expect(run(func(tx api.Transaction) error {
		return cat.SaveSchema(tx, tmpl.GenerateSchema("/db", "pub"), true)
	})).To(gomega.Succeed())

	g.Expect(run(func(tx api.Transaction) error {
		ok, err := cat.DoesSchemaExist(tx, "/db", "pub")
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(ok).To(gomega.BeTrue())
		return nil
	})).To(gomega.Succeed())

	g.Expect(run(func(tx api.Transaction) error {
		s, err := cat.LoadSchema(tx, "/db", "pub")
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(s.MetadataName()).To(gomega.Equal("pub"))
		g.Expect(s.DatabaseName()).To(gomega.Equal("/db"))
		g.Expect(s.SchemaTemplate().MetadataName()).To(gomega.Equal("schema-tmpl"))
		return nil
	})).To(gomega.Succeed())

	g.Expect(run(func(tx api.Transaction) error {
		return cat.DeleteSchema(tx, "/db", "pub")
	})).To(gomega.Succeed())

	err := run(func(tx api.Transaction) error {
		_, err := cat.LoadSchema(tx, "/db", "pub")
		return err
	})
	var apiErr *api.Error
	g.Expect(errors.As(err, &apiErr)).To(gomega.BeTrue())
	g.Expect(apiErr.Code).To(gomega.Equal(api.ErrCodeUndefinedSchema))
}

// TestFDB_SaveSchemaWithoutDatabase: createDatabaseIfNecessary=false
// when the database row is missing → ErrCodeUndefinedDatabase.
func TestFDB_SaveSchemaWithoutDatabase(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	cat, run := newFDBCatalogInSubspace(t)
	tc := cat.SchemaTemplateCatalog()
	tmpl := buildVersionedTemplate(t, "no-db-tmpl", 1)

	g.Expect(run(func(tx api.Transaction) error {
		return tc.CreateTemplate(tx, tmpl)
	})).To(gomega.Succeed())

	err := run(func(tx api.Transaction) error {
		return cat.SaveSchema(tx, tmpl.GenerateSchema("/no-such-db", "pub"), false)
	})
	var apiErr *api.Error
	g.Expect(errors.As(err, &apiErr)).To(gomega.BeTrue())
	g.Expect(apiErr.Code).To(gomega.Equal(api.ErrCodeUndefinedDatabase))
}

// TestFDB_SaveSchemaWithUnknownTemplate: template not registered →
// ErrCodeUnknownSchemaTemplate.
func TestFDB_SaveSchemaWithUnknownTemplate(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	cat, run := newFDBCatalogInSubspace(t)
	tmpl := buildVersionedTemplate(t, "ghost-tmpl", 1)

	err := run(func(tx api.Transaction) error {
		return cat.SaveSchema(tx, tmpl.GenerateSchema("/db", "pub"), true)
	})
	var apiErr *api.Error
	g.Expect(errors.As(err, &apiErr)).To(gomega.BeTrue())
	g.Expect(apiErr.Code).To(gomega.Equal(api.ErrCodeUnknownSchemaTemplate))
}

// TestFDB_ListSchemasInDatabase: multi-schema listing with per-db filter.
func TestFDB_ListSchemasInDatabase(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	cat, run := newFDBCatalogInSubspace(t)
	tc := cat.SchemaTemplateCatalog()
	tmpl := buildVersionedTemplate(t, "list-tmpl", 1)

	g.Expect(run(func(tx api.Transaction) error {
		g.Expect(tc.CreateTemplate(tx, tmpl)).To(gomega.Succeed())
		g.Expect(cat.SaveSchema(tx, tmpl.GenerateSchema("/db1", "s1"), true)).To(gomega.Succeed())
		g.Expect(cat.SaveSchema(tx, tmpl.GenerateSchema("/db1", "s2"), true)).To(gomega.Succeed())
		g.Expect(cat.SaveSchema(tx, tmpl.GenerateSchema("/db2", "s1"), true)).To(gomega.Succeed())
		return nil
	})).To(gomega.Succeed())

	g.Expect(run(func(tx api.Transaction) error {
		rs, err := cat.ListSchemasInDatabase(tx, "/db1", nil)
		g.Expect(err).ToNot(gomega.HaveOccurred())
		defer rs.Close()
		var got []string
		for rs.Next() {
			name, _ := rs.String(2) // SCHEMA_NAME
			got = append(got, name)
		}
		g.Expect(rs.Err()).ToNot(gomega.HaveOccurred())
		g.Expect(got).To(gomega.ConsistOf("s1", "s2"))
		return nil
	})).To(gomega.Succeed())

	g.Expect(run(func(tx api.Transaction) error {
		rs, err := cat.ListSchemas(tx, nil)
		g.Expect(err).ToNot(gomega.HaveOccurred())
		defer rs.Close()
		count := 0
		for rs.Next() {
			count++
		}
		g.Expect(rs.Err()).ToNot(gomega.HaveOccurred())
		g.Expect(count).To(gomega.Equal(3))
		return nil
	})).To(gomega.Succeed())
}

// TestFDB_RepairSchema rebinds a schema to the latest template version.
func TestFDB_RepairSchema(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	cat, run := newFDBCatalogInSubspace(t)
	tc := cat.SchemaTemplateCatalog()

	tmpl1 := buildVersionedTemplate(t, "repair-tmpl", 1)
	tmpl2 := buildVersionedTemplate(t, "repair-tmpl", 2)

	g.Expect(run(func(tx api.Transaction) error {
		g.Expect(tc.CreateTemplate(tx, tmpl1)).To(gomega.Succeed())
		return cat.SaveSchema(tx, tmpl1.GenerateSchema("/db", "pub"), true)
	})).To(gomega.Succeed())

	g.Expect(run(func(tx api.Transaction) error {
		return tc.CreateTemplate(tx, tmpl2)
	})).To(gomega.Succeed())

	g.Expect(run(func(tx api.Transaction) error {
		return cat.RepairSchema(tx, "/db", "pub")
	})).To(gomega.Succeed())

	g.Expect(run(func(tx api.Transaction) error {
		s, err := cat.LoadSchema(tx, "/db", "pub")
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(s.SchemaTemplate().Version()).To(gomega.Equal(2))
		return nil
	})).To(gomega.Succeed())
}

// TestFDB_DeleteSchemaNotFound: deleting an absent schema →
// ErrCodeUndefinedSchema.
func TestFDB_DeleteSchemaNotFound(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	cat, run := newFDBCatalogInSubspace(t)

	err := run(func(tx api.Transaction) error {
		return cat.DeleteSchema(tx, "/db", "ghost")
	})
	var apiErr *api.Error
	g.Expect(errors.As(err, &apiErr)).To(gomega.BeTrue())
	g.Expect(apiErr.Code).To(gomega.Equal(api.ErrCodeUndefinedSchema))
}

// TestFDB_LoadSchemaNotFound: loading an absent schema →
// ErrCodeUndefinedSchema.
func TestFDB_LoadSchemaNotFound(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	cat, run := newFDBCatalogInSubspace(t)

	err := run(func(tx api.Transaction) error {
		_, err := cat.LoadSchema(tx, "/db", "ghost")
		return err
	})
	var apiErr *api.Error
	g.Expect(errors.As(err, &apiErr)).To(gomega.BeTrue())
	g.Expect(apiErr.Code).To(gomega.Equal(api.ErrCodeUndefinedSchema))
}

// TestFDB_DeleteTemplateVersionExact deletes a single version while
// leaving others intact.
func TestFDB_DeleteTemplateVersionExact(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	cat, run := newFDBCatalogInSubspace(t)
	tc := cat.SchemaTemplateCatalog()

	tmpl1 := buildVersionedTemplate(t, "del-ver", 1)
	tmpl2 := buildVersionedTemplate(t, "del-ver", 2)

	g.Expect(run(func(tx api.Transaction) error {
		g.Expect(tc.CreateTemplate(tx, tmpl1)).To(gomega.Succeed())
		return tc.CreateTemplate(tx, tmpl2)
	})).To(gomega.Succeed())

	g.Expect(run(func(tx api.Transaction) error {
		return tc.DeleteTemplateVersion(tx, "del-ver", 1, true)
	})).To(gomega.Succeed())

	g.Expect(run(func(tx api.Transaction) error {
		ok, err := tc.DoesSchemaTemplateExistAtVersion(tx, "del-ver", 1)
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(ok).To(gomega.BeFalse())

		ok2, err := tc.DoesSchemaTemplateExistAtVersion(tx, "del-ver", 2)
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(ok2).To(gomega.BeTrue())
		return nil
	})).To(gomega.Succeed())
}

// TestFDB_ListTemplates enumerates persisted templates by name.
func TestFDB_ListTemplates(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	cat, run := newFDBCatalogInSubspace(t)
	tc := cat.SchemaTemplateCatalog()

	tmplA := buildVersionedTemplate(t, "list-A", 1)
	tmplB := buildVersionedTemplate(t, "list-B", 1)

	g.Expect(run(func(tx api.Transaction) error {
		g.Expect(tc.CreateTemplate(tx, tmplA)).To(gomega.Succeed())
		return tc.CreateTemplate(tx, tmplB)
	})).To(gomega.Succeed())

	g.Expect(run(func(tx api.Transaction) error {
		rs, err := tc.ListTemplates(tx)
		g.Expect(err).ToNot(gomega.HaveOccurred())
		defer rs.Close()
		var names []string
		for rs.Next() {
			name, _ := rs.String(1)
			names = append(names, name)
		}
		g.Expect(rs.Err()).ToNot(gomega.HaveOccurred())
		g.Expect(names).To(gomega.ConsistOf("list-A", "list-B"))
		return nil
	})).To(gomega.Succeed())
}

// TestFDB_ClosedTransactionRejected: catalog ops on a closed
// FDBTransaction return ErrCodeTransactionInactive.
func TestFDB_ClosedTransactionRejected(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)

	ctx30s, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := testFDB.Run(ctx30s, func(ctx *recordlayer.FDBRecordContext) (any, error) {
		tx := NewFDBTransaction(ctx)
		_ = tx.Close()

		cat, cerr := NewRecordLayerStoreCatalog(subspace.Sub([]byte("closed-tx-test")))
		g.Expect(cerr).ToNot(gomega.HaveOccurred())

		_, opErr := cat.DoesDatabaseExist(tx, "/db")
		var apiErr *api.Error
		g.Expect(errors.As(opErr, &apiErr)).To(gomega.BeTrue())
		g.Expect(apiErr.Code).To(gomega.Equal(api.ErrCodeTransactionInactive))
		return nil, nil
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())
}

// TestFDB_TemplateRoundTripPreservesSchema ensures that LoadSchemaTemplate
// after CreateTemplate round-trips the RecordMetaData correctly — the
// schema's tables must match the original.
func TestFDB_TemplateRoundTripPreservesSchema(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	cat, run := newFDBCatalogInSubspace(t)
	tc := cat.SchemaTemplateCatalog()
	tmpl := buildVersionedTemplate(t, "roundtrip-tmpl", 1)

	g.Expect(run(func(tx api.Transaction) error {
		return tc.CreateTemplate(tx, tmpl)
	})).To(gomega.Succeed())

	g.Expect(run(func(tx api.Transaction) error {
		got, err := tc.LoadSchemaTemplate(tx, "roundtrip-tmpl")
		g.Expect(err).ToNot(gomega.HaveOccurred())

		// Original template has 3 record types; round-tripped
		// template's generated schema must have the same tables.
		s := got.GenerateSchema("/db", "pub")
		tables, err := s.Tables()
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(tables).To(gomega.HaveLen(3))
		return nil
	})).To(gomega.Succeed())
}

// TestFDB_DeleteDatabase deletes a database and all its schemas, then
// confirms nothing remains.
func TestFDB_DeleteDatabase(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	cat, run := newFDBCatalogInSubspace(t)
	tc := cat.SchemaTemplateCatalog()
	tmpl := buildVersionedTemplate(t, "deldb-tmpl", 1)

	g.Expect(run(func(tx api.Transaction) error {
		g.Expect(tc.CreateTemplate(tx, tmpl)).To(gomega.Succeed())
		g.Expect(cat.SaveSchema(tx, tmpl.GenerateSchema("/deldb", "s1"), true)).To(gomega.Succeed())
		g.Expect(cat.SaveSchema(tx, tmpl.GenerateSchema("/deldb", "s2"), true)).To(gomega.Succeed())
		return nil
	})).To(gomega.Succeed())

	g.Expect(run(func(tx api.Transaction) error {
		ok, err := cat.DeleteDatabase(tx, "/deldb", true)
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(ok).To(gomega.BeTrue())
		return nil
	})).To(gomega.Succeed())

	// Database and schemas are gone.
	g.Expect(run(func(tx api.Transaction) error {
		dbOK, err := cat.DoesDatabaseExist(tx, "/deldb")
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(dbOK).To(gomega.BeFalse())

		s1OK, err := cat.DoesSchemaExist(tx, "/deldb", "s1")
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(s1OK).To(gomega.BeFalse())

		s2OK, err := cat.DoesSchemaExist(tx, "/deldb", "s2")
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(s2OK).To(gomega.BeFalse())
		return nil
	})).To(gomega.Succeed())
}

// TestFDB_DeleteDatabaseNotFound: throwIfDoesNotExist=true on an
// absent database → ErrCodeUnknownDatabase.
func TestFDB_DeleteDatabaseNotFound(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	cat, run := newFDBCatalogInSubspace(t)

	err := run(func(tx api.Transaction) error {
		_, err := cat.DeleteDatabase(tx, "/no-such", true)
		return err
	})
	var apiErr *api.Error
	g.Expect(errors.As(err, &apiErr)).To(gomega.BeTrue())
	g.Expect(apiErr.Code).To(gomega.Equal(api.ErrCodeUnknownDatabase))
}

// TestFDB_DeleteDatabaseSilentOnMissing: throwIfDoesNotExist=false on
// absent database returns (true, nil) — same as Java which relies on
// deleteRecord returning false silently.
func TestFDB_DeleteDatabaseSilentOnMissing(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	cat, run := newFDBCatalogInSubspace(t)

	g.Expect(run(func(tx api.Transaction) error {
		ok, err := cat.DeleteDatabase(tx, "/no-such", false)
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(ok).To(gomega.BeTrue())
		return nil
	})).To(gomega.Succeed())
}

// TestFDB_Initialize verifies that Initialize bootstraps the catalog's
// self-referential entries (template + sys database + CATALOG schema).
// Mirrors Java's RecordLayerStoreCatalogTestBase.testListSchemasEmptyResult
// which asserts /__SYS?schema=CATALOG is present after init.
func TestFDB_Initialize(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	cat, run := newFDBCatalogInSubspace(t)

	// Initialize.
	g.Expect(run(func(tx api.Transaction) error {
		return cat.Initialize(tx)
	})).To(gomega.Succeed())

	// Template CATALOG_TEMPLATE v1 must exist.
	g.Expect(run(func(tx api.Transaction) error {
		ok, err := cat.SchemaTemplateCatalog().DoesSchemaTemplateExistAtVersion(tx, CatalogTemplateName, CatalogTemplateVersion)
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(ok).To(gomega.BeTrue())
		return nil
	})).To(gomega.Succeed())

	// /__SYS database must exist.
	g.Expect(run(func(tx api.Transaction) error {
		ok, err := cat.DoesDatabaseExist(tx, SysDatabaseID)
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(ok).To(gomega.BeTrue())
		return nil
	})).To(gomega.Succeed())

	// /__SYS/CATALOG schema must exist.
	g.Expect(run(func(tx api.Transaction) error {
		ok, err := cat.DoesSchemaExist(tx, SysDatabaseID, CatalogConstant)
		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(ok).To(gomega.BeTrue())
		return nil
	})).To(gomega.Succeed())

	// Initialize is idempotent.
	g.Expect(run(func(tx api.Transaction) error {
		return cat.Initialize(tx)
	})).To(gomega.Succeed())

	// ListSchemas shows only the catalog schema.
	g.Expect(run(func(tx api.Transaction) error {
		rs, err := cat.ListSchemas(tx, nil)
		g.Expect(err).ToNot(gomega.HaveOccurred())
		defer rs.Close()
		var schemas []string
		for rs.Next() {
			db, _ := rs.String(1)
			name, _ := rs.String(2)
			schemas = append(schemas, db+"?schema="+name)
		}
		g.Expect(rs.Err()).ToNot(gomega.HaveOccurred())
		g.Expect(schemas).To(gomega.ConsistOf(SysDatabaseID + "?schema=" + CatalogConstant))
		return nil
	})).To(gomega.Succeed())
}
